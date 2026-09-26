package reviewruntime

import (
	"context"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// collectChildUsage дополняет root tokenUsage фактической публичной разбивкой
// отдельных дочерних thread. Один thread учитывается единожды. Root notification
// уже относится к собственному thread и не перечитывается как billing aggregate.
// Неизвестный тариф или задержка billing route сохраняет неполноту результата.
func (s *stageSession) collectChildUsage() {
	if !s.childSeen || s.engine.RunCodex != nil {
		return
	}
	r, err := s.engine.Store.Load(s.id)
	if err != nil {
		return
	}
	var attempt reviewstore.Attempt
	for _, stage := range r.Stages {
		if stage.ID == s.stage {
			attempt = stage.Attempts[len(stage.Attempts)-1]
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	observer, err := codex.OpenObserver(ctx, codex.Connection{Executable: s.engine.Executable, CWD: r.CWD})
	if err != nil {
		return
	}
	defer observer.Close()
	seen := map[string]bool{attempt.ThreadID: true}
	records := append([]reviewstore.ThreadUsage{}, attempt.ThreadUsage...)
	model, effort := r.Config.Review.Model, r.Config.Review.Effort
	if s.stage == reviewstore.ContextStage {
		model, effort = r.Config.Context.Model, r.Config.Context.Effort
	}
	if s.stage == reviewstore.PresentationStage {
		model, effort = r.Config.Presentation.Model, r.Config.Presentation.Effort
	}
	parent := attempt.Usage
	p, ok := r.Config.Prices[model]
	if ok {
		parent = reviewstore.PriceUsage(parent, &p)
	}
	rootPresent := false
	for _, g := range records {
		if g.ThreadID == attempt.ThreadID {
			rootPresent = true
		}
	}
	if !rootPresent {
		records = append(records, reviewstore.ThreadUsage{ThreadID: attempt.ThreadID, Model: model, Effort: effort, Usage: parent})
	}
	complete := true
	for _, activity := range r.Activities {
		if activity.Kind != "subagent" || activity.Stage != s.stage || activity.Attempt != s.number || activity.ThreadID == "" || seen[activity.ThreadID] {
			continue
		}
		seen[activity.ThreadID] = true
		already := false
		for _, g := range records {
			if g.ThreadID == activity.ThreadID {
				already = true
			}
		}
		if already {
			continue
		}
		if info, err := observer.ReadChildInfo(activity.ThreadID, activity.ParentThreadID); err == nil {
			path := ""
			if info.Prompt != "" && activity.DocumentPath == "" {
				path = "artifacts/agents/native-" + activity.ThreadID + ".md"
				if s.engine.Store.SaveArtifact(s.id, path, []byte(info.Prompt)) != nil {
					path = ""
				}
			}
			_, _ = s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
				for i := range r.Activities {
					if r.Activities[i].ID == activity.ID {
						r.Activities[i].Model = info.Model
						r.Activities[i].Effort = info.Effort
						if path != "" {
							r.Activities[i].DocumentPath = path
						}
					}
				}
				return nil
			})
		}
		groups, err := observer.ReadUsage(activity.ThreadID)
		if err != nil {
			complete = false
			continue
		}
		for _, g := range groups {
			if g.Model == nil || *g.Model == "" {
				complete = false
				continue
			}
			effort := ""
			if g.ReasoningEffort != nil {
				effort = *g.ReasoningEffort
			}
			u := reviewstore.Usage{InputTokens: g.InputTokens, CachedInputTokens: g.CachedInputTokens, OutputTokens: g.OutputTokens}
			var price *reviewstore.Pricing
			if p, ok := r.Config.Prices[*g.Model]; ok {
				price = &p
			}
			u = reviewstore.PriceUsage(u, price)
			records = append(records, reviewstore.ThreadUsage{ThreadID: activity.ThreadID, Model: *g.Model, Effort: effort, Usage: u})
		}
	}
	if len(seen) == 1 {
		complete = false
	}
	aggregate := aggregateUsage(records, complete)
	_ = s.engine.mutateStage(s.id, s.stage, func(_ *reviewstore.Review, _ *reviewstore.Stage, a *reviewstore.Attempt) error {
		a.ThreadUsage = records
		a.Usage = aggregate
		return nil
	})
}

// aggregateUsage складывает раздельные thread/model-группы; известные токены
// остаются доступны при неполноте, но частичная стоимость не выдаётся за итог.
func aggregateUsage(records []reviewstore.ThreadUsage, complete bool) reviewstore.Usage {
	var input, cache, output int64
	var cost float64
	knownInput, knownCache, knownOutput := false, false, false
	for _, r := range records {
		u := r.Usage
		if u.InputTokens != nil {
			input += *u.InputTokens
			knownInput = true
		} else {
			complete = false
		}
		if u.CachedInputTokens != nil {
			cache += *u.CachedInputTokens
			knownCache = true
		} else {
			complete = false
		}
		if u.OutputTokens != nil {
			output += *u.OutputTokens
			knownOutput = true
		} else {
			complete = false
		}
		if u.CostUSD != nil {
			cost += *u.CostUSD
		} else {
			complete = false
		}
		if !u.Complete {
			complete = false
		}
	}
	u := reviewstore.Usage{Complete: complete}
	if knownInput {
		u.InputTokens = &input
	}
	if knownCache {
		u.CachedInputTokens = &cache
	}
	if knownOutput {
		u.OutputTokens = &output
	}
	if complete {
		u.CostUSD = &cost
	}
	return u
}
