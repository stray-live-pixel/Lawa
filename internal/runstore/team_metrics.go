package runstore

import (
	"fmt"
	"slices"
	"time"
)

// TeamMetrics дополняет существующие сообщения и кадры только фактами исполнения.
// Живёт в team.json под тем же lock; никогда не передаётся в модельный контекст.
// RecordedFrom обозначает начало наблюдений, а не начало старого заказа.
type TeamMetrics struct {
	RecordedFrom time.Time        `json:"recordedFrom"`
	Executions   []*TeamExecution `json:"executions"`
}

// TeamExecution — одна попытка доставки. Ключ actor/attempt переживает рестарт;
// отсутствие границы означает неизвестное время, а не нулевую длительность.
// FinishedAt известен только из живого терминального события. После аварии
// ObservedFinishedAt фиксирует наблюдение, не выдавая время простоя за работу.
type TeamExecution struct {
	NewThread          bool            `json:"newThread,omitempty"`
	ID                 string          `json:"id"`
	ActorID            string          `json:"actorId"`
	Attempt            uint64          `json:"attempt"`
	MessageIDs         []string        `json:"messageIds"`
	ThreadID           string          `json:"threadId,omitempty"`
	TurnID             string          `json:"turnId,omitempty"`
	ClaimedAt          *time.Time      `json:"claimedAt"`
	StartedAt          *time.Time      `json:"startedAt"`
	FinishedAt         *time.Time      `json:"finishedAt"`
	ObservedFinishedAt *time.Time      `json:"observedFinishedAt"`
	Outcome            string          `json:"outcome,omitempty"`
	Usage              *TeamTokenUsage `json:"usage"`
}

// TeamTokenUsage сохраняет разрешённые счётчики исходного уведомления.
// Total накоплен по thread, Last относится к последнему запросу модели, НЕ ко
// всему turn. Их нельзя складывать между исполнениями или выдавать за цену.
type TeamTokenUsage struct {
	At    time.Time        `json:"at"`
	Total *TeamTokenCounts `json:"threadTotal"`
	Last  *TeamTokenCounts `json:"lastRequest"`
}

// TeamTokenCounts не принимает произвольные числовые поля инструмента/сервера.
// Указатели сохраняют разницу между отсутствующим счётчиком и настоящим нулём.
type TeamTokenCounts struct {
	Total           *int64 `json:"totalTokens"`
	Input           *int64 `json:"inputTokens"`
	CachedInput     *int64 `json:"cachedInputTokens"`
	Output          *int64 `json:"outputTokens"`
	CacheWriteInput *int64 `json:"cacheWriteInputTokens"`
	ReasoningOutput *int64 `json:"reasoningOutputTokens"`
}

// Valid проверяет только известные счётчики; пустой/повреждённый usage неизвестен.
func (c *TeamTokenCounts) Valid() bool {
	if c == nil {
		return false
	}
	known := false
	for _, n := range []*int64{c.Total, c.Input, c.CachedInput, c.Output, c.CacheWriteInput, c.ReasoningOutput} {
		if n != nil {
			if *n < 0 {
				return false
			}
			known = true
		}
	}
	return known
}

// TeamExecutionFor возвращает запись текущей доставки под team.lock. Для старой
// незавершённой доставки создаётся запись без выдуманных времени начала и claim.
func TeamExecutionFor(chat *TeamChat, actorID string, now time.Time) *TeamExecution {
	if chat.Room == nil {
		return nil
	}
	a := chat.Room.Actors[actorID]
	if a == nil || a.Delivery == nil {
		return nil
	}
	if chat.Metrics == nil {
		chat.Metrics = &TeamMetrics{RecordedFrom: now.UTC()}
	}
	id := fmt.Sprintf("%s/%d", actorID, a.Attempt)
	for _, execution := range chat.Metrics.Executions {
		if execution.ID == id {
			return execution
		}
	}
	execution := &TeamExecution{ID: id, ActorID: actorID, Attempt: a.Attempt, MessageIDs: slices.Clone(a.Delivery.IDs), ThreadID: a.ThreadID, TurnID: a.Delivery.TurnID}
	chat.Metrics.Executions = append(chat.Metrics.Executions, execution)
	return execution
}

// StartTeamExecution фиксирует подтверждение turn/start до дальнейшего чтения
// протокола. Повтор callback сохраняет исходную границу, не открывая интервал заново.
func StartTeamExecution(chat *TeamChat, actorID string, now time.Time) {
	execution := TeamExecutionFor(chat, actorID, now)
	if execution == nil {
		return
	}
	a := chat.Room.Actors[actorID]
	execution.ThreadID, execution.TurnID = a.ThreadID, a.Delivery.TurnID
	if execution.StartedAt == nil {
		at := now.UTC()
		execution.StartedAt = &at
	}
}

// FinishTeamExecution закрывает попытку один раз. Наблюдение после рестарта
// не заменяет сохранённое терминальное событие и не создаёт второй интервал.
func FinishTeamExecution(chat *TeamChat, actorID, outcome string, now time.Time, observed bool) {
	execution := TeamExecutionFor(chat, actorID, now)
	if execution == nil {
		return
	}
	at := now.UTC()
	if observed {
		if execution.ObservedFinishedAt == nil {
			execution.ObservedFinishedAt = &at
		}
	} else if execution.FinishedAt == nil {
		execution.FinishedAt = &at
	}
	if execution.Outcome == "" {
		execution.Outcome = outcome
	}
}

// validateTeamMetrics отклоняет повреждённые записи до отчёта и runtime:
// null или повтор одной попытки не должны вызывать panic либо удваивать время.
func validateTeamMetrics(metrics *TeamMetrics) error {
	if metrics == nil {
		return nil
	}
	ids, attempts := map[string]bool{}, map[string]bool{}
	for _, e := range metrics.Executions {
		if e == nil {
			return fmt.Errorf("повреждены метрики: пустое исполнение")
		}
		key := fmt.Sprintf("%s/%d", e.ActorID, e.Attempt)
		if e.ID == "" || e.ActorID == "" || ids[e.ID] || attempts[key] {
			return fmt.Errorf("повреждены метрики: пустая или повторная попытка")
		}
		ids[e.ID], attempts[key] = true, true
		if e.Usage != nil && (e.Usage.Total != nil && !e.Usage.Total.Valid() || e.Usage.Last != nil && !e.Usage.Last.Valid()) {
			return fmt.Errorf("повреждены метрики: неверные счётчики")
		}
	}
	return nil
}
