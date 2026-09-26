package reviewruntime

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// observe сохраняет публичные события без приватных рассуждений. Счётчики total
// заменяются, а не складываются на каждом обновлении, иначе usage завышается.
func (s *stageSession) observe(event codex.Event) error {
	// Дельты UI и квоты аккаунта не меняют review. Запись завершённого item
	// сохраняет полный журнал без сотен fsync и ложного «обновлено» на heartbeat.
	if event.Method != "thread/tokenUsage/updated" && event.Method != "item/started" && event.Method != "item/completed" && event.Method != "turn/completed" && event.Method != "turn/started" && event.Method != "error" {
		return nil
	}
	s.eventNumber++
	key := fmt.Sprintf("%s-%d-%06d", s.stage, s.number, s.eventNumber)
	if s.eventPrefix != "" {
		key = s.eventPrefix + "-" + key
	}
	var params any
	if json.Unmarshal(event.Params, &params) != nil {
		params = string(event.Params)
	}
	if p, ok := params.(map[string]any); ok {
		if item, ok := p["item"].(map[string]any); ok && item["type"] == "reasoning" {
			return nil
		}
	}
	if err := s.engine.Store.AppendEvent(s.id, reviewstore.Event{ID: key, At: time.Now().UTC(), Stage: s.stage, Kind: event.Method, Data: params}); err != nil {
		return err
	}
	if event.Method == "thread/tokenUsage/updated" {
		return s.usage(event.Params)
	}
	if event.Method != "item/completed" && event.Method != "item/started" {
		return nil
	}
	var data struct {
		ThreadID string `json:"threadId"`
		Item     struct {
			ID                string   `json:"id"`
			Type              string   `json:"type"`
			Text              string   `json:"text"`
			Command           string   `json:"command"`
			CWD               string   `json:"cwd"`
			AggregatedOutput  string   `json:"aggregatedOutput"`
			ExitCode          *int     `json:"exitCode"`
			Tool              string   `json:"tool"`
			Status            string   `json:"status"`
			Model             string   `json:"model"`
			ReasoningEffort   string   `json:"reasoningEffort"`
			Prompt            string   `json:"prompt"`
			SenderThreadID    string   `json:"senderThreadId"`
			ReceiverThreadIDs []string `json:"receiverThreadIds"`
			AgentsStates      map[string]struct {
				Status string `json:"status"`
			} `json:"agentsStates"`
			AgentThreadID  string `json:"agentThreadId"`
			AgentPath      string `json:"agentPath"`
			Kind           string `json:"kind"`
			CommandActions []struct {
				Type string `json:"type"`
				Path string `json:"path"`
			} `json:"commandActions"`
		} `json:"item"`
	}
	if json.Unmarshal(event.Params, &data) != nil {
		return nil
	}
	item := data.Item
	if event.Method == "item/completed" && item.Type == "agentMessage" {
		s.output += item.Text + "\n"
	}
	if event.Method == "item/completed" && item.Type == "commandExecution" && item.ExitCode != nil && *item.ExitCode == 0 && item.AggregatedOutput != "" {
		for i, action := range item.CommandActions {
			if action.Type != "read" || filepath.Base(action.Path) != "SKILL.md" {
				continue
			}
			// Сохраняем именно увиденный вывод, а не более позднюю версию файла.
			path := fmt.Sprintf("artifacts/skills/observed-%s-%d.md", key, i)
			if err := s.engine.Store.SaveArtifact(s.id, path, []byte(item.AggregatedOutput)); err != nil {
				return err
			}
			if _, err := s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
				r.Activities = append(r.Activities, reviewstore.Activity{ID: fmt.Sprintf("skill-%s-%d", key, i), Stage: s.stage, Attempt: s.number, Kind: "skill", Title: filepath.Base(filepath.Dir(action.Path)), State: "read", DocumentPath: path, ThreadID: data.ThreadID, At: time.Now().UTC()})
				return nil
			}); err != nil {
				return err
			}
		}
	}
	if event.Method == "item/completed" && item.Type == "commandExecution" && s.stage == reviewstore.ContextStage {
		path := "artifacts/commands/" + key + ".txt"
		output := item.AggregatedOutput
		if output == "" {
			output = "(нет вывода)\n"
		}
		if err := s.engine.Store.SaveArtifact(s.id, path, []byte(output)); err != nil {
			return err
		}
		_, err := s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
			r.Context.Commands = append(r.Context.Commands, reviewstore.Command{ID: "observed-" + key, Command: item.Command, CWD: item.CWD, OutputPath: path, ExitCode: item.ExitCode, ExecutedAt: time.Now().UTC()})
			return nil
		})
		return err
	}
	if item.Type == "collabAgentToolCall" && item.Tool == "spawnAgent" {
		if len(item.ReceiverThreadIDs) > 0 {
			s.childSeen = true
		}
		if s.childSeen {
			if err := s.engine.mutateStage(s.id, s.stage, func(_ *reviewstore.Review, _ *reviewstore.Stage, a *reviewstore.Attempt) error {
				a.Usage.Complete = false
				a.Usage.CostUSD = nil
				return nil
			}); err != nil {
				return err
			}
		}
		path := ""
		if item.Prompt != "" {
			path = "artifacts/agents/" + key + ".md"
			if err := s.engine.Store.SaveArtifact(s.id, path, []byte(item.Prompt)); err != nil {
				return err
			}
		}
		_, err := s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
			id := "subagent-" + item.ID
			activity := reviewstore.Activity{Stage: s.stage, Attempt: s.number, Model: item.Model, Effort: item.ReasoningEffort, ID: id, Kind: "subagent", Title: "Субагент", State: "pendingInit", DocumentPath: path, ParentThreadID: item.SenderThreadID, At: time.Now().UTC()}
			if item.Status == "failed" || item.Status == "interrupted" {
				activity.State = item.Status
			}
			if len(item.ReceiverThreadIDs) > 0 {
				activity.ThreadID = item.ReceiverThreadIDs[0]
				activity.State = "running"
				if state, ok := item.AgentsStates[activity.ThreadID]; ok {
					activity.State = state.Status
				}
			}
			for i := range r.Activities {
				if r.Activities[i].ID == id {
					if activity.DocumentPath == "" {
						activity.DocumentPath = r.Activities[i].DocumentPath
					}
					r.Activities[i] = activity
					return nil
				}
			}
			r.Activities = append(r.Activities, activity)
			return nil
		})
		return err
	}
	if item.Type == "subAgentActivity" && item.Kind != "interacted" && item.AgentThreadID != "" {
		// Новые Codex присылают только subAgentActivity, без spawnAgent item.
		// Interacted может указывать назад на родителя и не объявляет ребёнка.
		s.childSeen = true
		return s.engine.mutateStage(s.id, s.stage, func(r *reviewstore.Review, _ *reviewstore.Stage, attempt *reviewstore.Attempt) error {
			if item.AgentThreadID == attempt.ThreadID {
				return nil
			}
			attempt.Usage.Complete = false
			attempt.Usage.CostUSD = nil
			state := "running"
			if item.Kind == "completed" || item.Kind == "interrupted" {
				state = item.Kind
			}
			for i := range r.Activities {
				a := &r.Activities[i]
				if a.Kind == "subagent" && a.Stage == s.stage && a.Attempt == s.number && a.ThreadID == item.AgentThreadID {
					a.State = state
					return nil
				}
			}
			title := item.AgentPath
			if title == "" {
				title = "Субагент"
			}
			r.Activities = append(r.Activities, reviewstore.Activity{ID: "subagent-" + item.AgentThreadID, Stage: s.stage, Attempt: s.number, Kind: "subagent", Title: title, State: state, ThreadID: item.AgentThreadID, ParentThreadID: data.ThreadID, At: time.Now().UTC()})
			return nil
		})
	}
	if item.Type == "collabAgentToolCall" || item.Type == "subAgentActivity" {
		_, err := s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
			for i := range r.Activities {
				a := &r.Activities[i]
				if a.Kind != "subagent" {
					continue
				}
				if state, ok := item.AgentsStates[a.ThreadID]; ok {
					a.State = state.Status
				}
				if item.AgentThreadID == a.ThreadID {
					switch item.Kind {
					case "started", "interacted":
						a.State = "running"
					case "completed", "interrupted":
						a.State = item.Kind
					}
				}
			}
			return nil
		})
		return err
	}
	return nil
}

// usage принимает только счётчики текущего thread. Input включает cache, поэтому
// для API-эквивалента cached вычитается из обычного input; неизвестное — nil.
func (s *stageSession) usage(raw []byte) error {
	var d struct {
		ThreadID   string `json:"threadId"`
		TokenUsage struct {
			Total *struct {
				Input  *int64 `json:"inputTokens"`
				Cache  *int64 `json:"cachedInputTokens"`
				Output *int64 `json:"outputTokens"`
			} `json:"total"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(raw, &d) != nil || d.TokenUsage.Total == nil {
		return nil
	}
	u := d.TokenUsage.Total
	for _, n := range []*int64{u.Input, u.Cache, u.Output} {
		if n != nil && *n < 0 {
			return nil
		}
	}
	if u.Input != nil && u.Cache != nil && *u.Cache > *u.Input {
		return nil
	}
	return s.engine.mutateStage(s.id, s.stage, func(r *reviewstore.Review, _ *reviewstore.Stage, a *reviewstore.Attempt) error {
		if a.ThreadID != d.ThreadID {
			return nil
		}
		usage := reviewstore.Usage{InputTokens: u.Input, CachedInputTokens: u.Cache, OutputTokens: u.Output}
		model := r.Config.Context.Model
		if s.stage == reviewstore.ReviewStage {
			model = r.Config.Review.Model
		}
		if s.stage == reviewstore.PresentationStage {
			model = r.Config.Presentation.Model
		}
		var price *reviewstore.Pricing
		if p, ok := r.Config.Prices[model]; ok {
			price = &p
		}
		usage = reviewstore.PriceUsage(usage, price)
		effort := r.Config.Context.Effort
		if s.stage == reviewstore.ReviewStage {
			effort = r.Config.Review.Effort
		}
		if s.stage == reviewstore.PresentationStage {
			effort = r.Config.Presentation.Effort
		}
		putThreadUsage(a, reviewstore.ThreadUsage{ThreadID: a.ThreadID, Model: model, Effort: effort, Usage: usage})
		a.Usage = stageAggregate(*r, *a, s.stage)
		return nil
	})
}
