package reviewruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// delegateInput описывает узкое поручение; отсутствие override наследует этап.
type delegateInput struct{ Title, Prompt, Model, Effort string }

// delegate владеет отдельным child thread и его точным входом. Синхронный вызов
// не оставляет бесхозных процессов: отмена родителя распространяется на ребёнка.
// Динамический инструмент фиксирует prompt до запуска, поэтому прозрачность
// не зависит от недоступной/зашифрованной истории native subagent.
func (s *stageSession) delegate(ctx context.Context, callID string, in delegateInput) (string, error) {
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Prompt) == "" {
		return "", errors.New("нужны Title и Prompt субагента")
	}
	r, err := s.engine.Store.Load(s.id)
	if err != nil {
		return "", err
	}
	config := r.Config.Review
	if in.Model != "" {
		config.Model = in.Model
	}
	if in.Effort != "" {
		config.Effort = in.Effort
	}
	if config != r.Config.Review && s.engine.RunCodex == nil {
		copy := r
		copy.Config.Review = config
		if err = s.engine.validateModels(ctx, copy); err != nil {
			return "", err
		}
	}
	key := fmt.Sprintf("delegate-%s-%d-%x", s.stage, s.number, sha256.Sum256([]byte(callID)))
	path := "artifacts/agents/" + key + ".md"
	out := "artifacts/agents/" + key + "-result.md"
	for _, a := range r.Activities {
		if a.ID == key {
			if a.State == "completed" {
				data, err := s.engine.Store.ReadArtifact(s.id, out)
				return string(data), err
			}
			return "", errors.New("этот вызов делегирования уже выполнялся; изучите его результат/ошибку")
		}
	}
	prompt := "Ты субагент code review. Выполни только порученную проверку и верни факты, воспроизведение и доказательства. Не меняй исходный код, не публикуй комментарии, не запускай других субагентов и не задавай вопросов. Используй правила проекта. Читай скиллы через review_read_skill.\n\n" + "Контекст review для чтения: " + filepath.Join(s.engine.Store.Root, s.id) + "\n\n" + in.Prompt
	if err = s.engine.Store.SaveArtifact(s.id, path, []byte(prompt)); err != nil {
		return "", err
	}
	parent := ""
	for _, stage := range r.Stages {
		if stage.ID == s.stage {
			parent = stage.Attempts[len(stage.Attempts)-1].ThreadID
		}
	}
	_, err = s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
		r.Activities = append(r.Activities, reviewstore.Activity{ID: key, Stage: s.stage, Attempt: s.number, Kind: "subagent", Title: in.Title, State: "running", DocumentPath: path, ParentThreadID: parent, Model: config.Model, Effort: config.Effort, At: time.Now().UTC()})
		return nil
	})
	if err != nil {
		return "", err
	}
	child := &stageSession{engine: s.engine, id: s.id, stage: s.stage, number: s.number, scratch: s.scratch, eventPrefix: key}
	thread := ""
	update := func(fn func(*reviewstore.Activity)) error {
		_, err := s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
			for i := range r.Activities {
				if r.Activities[i].ID == key {
					fn(&r.Activities[i])
					return nil
				}
			}
			return errors.New("субагент не найден")
		})
		return err
	}
	tools := []codex.DynamicTool{}
	for _, t := range child.tools() {
		if t.Name == "review_read_skill" {
			tools = append(tools, t)
		}
	}
	command := codex.Command{Executable: s.engine.Executable, CWD: r.CWD, Text: prompt, Title: in.Title, Model: config.Model, Effort: config.Effort, Permissions: &codex.PermissionProfile{Name: "lawa_review", ReadPaths: []string{filepath.Join(s.engine.Store.Root, s.id), r.CWD}, WritePaths: []string{s.scratch}}, DynamicTools: tools, CallDynamicTool: child.call,
		OnThread: func(id string) error {
			thread = id
			child.actorThreadID = id
			return update(func(a *reviewstore.Activity) { a.ThreadID = id })
		},
		Notify: func(event codex.Event) error {
			if event.Method == "thread/tokenUsage/updated" {
				var data struct {
					ThreadID   string
					TokenUsage struct {
						Total struct{ InputTokens, CachedInputTokens, OutputTokens *int64 }
					}
				}
				if json.Unmarshal(event.Params, &data) == nil && data.ThreadID == thread {
					u := reviewstore.Usage{InputTokens: data.TokenUsage.Total.InputTokens, CachedInputTokens: data.TokenUsage.Total.CachedInputTokens, OutputTokens: data.TokenUsage.Total.OutputTokens}
					var price *reviewstore.Pricing
					if p, ok := r.Config.Prices[config.Model]; ok {
						price = &p
					}
					u = reviewstore.PriceUsage(u, price)
					if err := s.engine.mutateStage(s.id, s.stage, func(r *reviewstore.Review, _ *reviewstore.Stage, a *reviewstore.Attempt) error {
						putThreadUsage(a, reviewstore.ThreadUsage{ThreadID: thread, Model: config.Model, Effort: config.Effort, Usage: u})
						a.Usage = stageAggregate(*r, *a, s.stage)
						return nil
					}); err != nil {
						return err
					}
				}
			}
			return child.observe(event)
		},
	}
	diagnostics := &diagnosticBuffer{}
	command.Stderr = diagnostics
	runner := s.engine.RunCodex
	if runner == nil {
		runner = codex.Run
	}
	result, runErr := runner(ctx, command)
	if diagnostics.Len() > 0 {
		_ = s.engine.Store.SaveArtifact(s.id, "artifacts/agents/"+key+"-stderr.log", diagnostics.Bytes())
	}
	if runErr == nil && (result.Status != "completed" || strings.TrimSpace(child.output) == "") {
		runErr = errors.New("субагент не вернул завершённый ответ")
	}
	if runErr == nil {
		runErr = s.engine.Store.SaveArtifact(s.id, out, []byte(child.output))
	}
	state := "completed"
	if runErr != nil {
		state = "failed"
	}
	if ctx.Err() != nil {
		state = "interrupted"
	}
	if err = update(func(a *reviewstore.Activity) { a.State = state }); err != nil {
		runErr = errors.Join(runErr, err)
	}
	_ = s.engine.mutateStage(s.id, s.stage, func(r *reviewstore.Review, _ *reviewstore.Stage, a *reviewstore.Attempt) error {
		a.Usage = stageAggregate(*r, *a, s.stage)
		return nil
	})
	return child.output, runErr
}

// putThreadUsage заменяет накопительные показатели одной модели в одном thread.
func putThreadUsage(a *reviewstore.Attempt, u reviewstore.ThreadUsage) {
	for i := range a.ThreadUsage {
		if a.ThreadUsage[i].ThreadID == u.ThreadID && a.ThreadUsage[i].Model == u.Model && a.ThreadUsage[i].Effort == u.Effort {
			a.ThreadUsage[i] = u
			return
		}
	}
	a.ThreadUsage = append(a.ThreadUsage, u)
}

// stageAggregate требует root и всех фактически запущенных детей. Неполнота
// отражается явно, даже если у родителя все три token-счётчика уже известны.
func stageAggregate(r reviewstore.Review, a reviewstore.Attempt, stage reviewstore.StageID) reviewstore.Usage {
	seen := map[string]bool{}
	for _, g := range a.ThreadUsage {
		seen[g.ThreadID] = true
	}
	complete := seen[a.ThreadID]
	for _, activity := range r.Activities {
		if activity.Kind != "subagent" || activity.Stage != stage || activity.Attempt != a.Number {
			continue
		}
		if activity.ThreadID != "" && !seen[activity.ThreadID] {
			complete = false
		}
		if activity.State == "running" || activity.State == "pendingInit" {
			complete = false
		}
	}
	return aggregateUsage(a.ThreadUsage, complete)
}
