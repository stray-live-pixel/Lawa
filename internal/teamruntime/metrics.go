package teamruntime

import (
	"context"
	"encoding/json/v2"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// metricTime использует те же управляемые часы, что и scheduler.
func (e *Engine) metricTime() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// finishMetric сохраняет границу до очистки доставки; повтор безопасен.
func (e *Engine) finishMetric(ctx context.Context, run, id, outcome string, observed bool) error {
	return runstore.UpdateTeam(ctx, e.Root, run, func(chat *runstore.TeamChat) error {
		runstore.FinishTeamExecution(chat, id, outcome, e.metricTime(), observed)
		return nil
	})
}

// metricEvent допускает только известные счётчики и терминальную границу своего
// turn. Аргументы/вывод инструментов, ошибки и прочие поля не копируются.
func (e *Engine) metricEvent(run, id string, event codex.Event) error {
	if event.Method != "thread/tokenUsage/updated" && event.Method != "turn/completed" {
		return nil
	}
	var data struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
		TokenUsage struct {
			Total *runstore.TeamTokenCounts `json:"total"`
			Last  *runstore.TeamTokenCounts `json:"last"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(event.Params, &data) != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runstore.UpdateTeam(ctx, e.Root, run, func(chat *runstore.TeamChat) error {
		a := chat.Room.Actors[id]
		if a == nil || a.Delivery == nil || a.ThreadID == "" || data.ThreadID != a.ThreadID || a.Delivery.TurnID == "" {
			return nil
		}
		if event.Method == "turn/completed" {
			if data.Turn.ID == a.Delivery.TurnID && (data.Turn.Status == "completed" || data.Turn.Status == "failed" || data.Turn.Status == "interrupted") {
				runstore.FinishTeamExecution(chat, id, data.Turn.Status, e.metricTime(), false)
			}
			return nil
		}
		if data.TurnID != a.Delivery.TurnID {
			return nil
		}
		if data.TokenUsage.Total != nil && !data.TokenUsage.Total.Valid() {
			return nil
		}
		if data.TokenUsage.Last != nil && !data.TokenUsage.Last.Valid() {
			return nil
		}
		if data.TokenUsage.Total == nil && data.TokenUsage.Last == nil {
			return nil
		}
		execution := runstore.TeamExecutionFor(chat, id, e.metricTime())
		execution.Usage = &runstore.TeamTokenUsage{At: e.metricTime(), Total: data.TokenUsage.Total, Last: data.TokenUsage.Last}
		return nil
	})
}
