// Package teamreport строит read-only отчёт по team.json без Codex и записи файлов.
package teamreport

import (
	"sort"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// Report отделяет календарные длительности от суммы агентных интервалов.
// Known* — только измеренная часть, не полный итог при неизвестных границах.
// Все длительности в секундах; nil сериализуется как null, а не ноль.
type Report struct {
	RunID                      string      `json:"runId"`
	AsOf                       time.Time   `json:"asOf"`
	RecordedFrom               *time.Time  `json:"recordedFrom"`
	FirstResultSeconds         *float64    `json:"firstResultSeconds"`
	FirstAcceptedSeconds       *float64    `json:"firstAcceptedSeconds"`
	WaitRecordedFrom           *time.Time  `json:"waitRecordedFrom"`
	CalendarSeconds            *float64    `json:"calendarSeconds"`
	ActiveAgentSeconds         *float64    `json:"activeAgentSeconds"`
	KnownActiveAgentSeconds    float64     `json:"knownActiveAgentSeconds"`
	KnownActiveCalendarSeconds float64     `json:"knownActiveCalendarSeconds"`
	UnknownExecutions          int         `json:"unknownExecutions"`
	Executions                 []Execution `json:"executions"`
	Tasks                      []Task      `json:"tasks"`
	Waits                      []Wait      `json:"waits"`
	Price                      *float64    `json:"price"`
}

// Execution связывает доступные счётчики с попыткой, не суммируя threadTotal.
type Execution struct {
	*runstore.TeamExecution
	ObservedTurnTokens *runstore.TeamTokenCounts `json:"observedTurnTokens"`
	ActiveSeconds      *float64                  `json:"activeSeconds"`
	Deliveries         []Delivery                `json:"deliveries"`
}

// Delivery измеряет время от сообщения до подтверждённого turn/start.
type Delivery struct {
	MessageID    string   `json:"messageId"`
	DelaySeconds *float64 `json:"delaySeconds"`
}

// Task показывает заявленную готовность, отдельную приёмку и явные возвраты.
type Task struct {
	ID                 string     `json:"id"`
	Assignee           string     `json:"assignee"`
	AssignedAt         time.Time  `json:"assignedAt"`
	FirstResultAt      *time.Time `json:"firstResultAt"`
	AcceptedAt         *time.Time `json:"acceptedAt"`
	FirstResultSeconds *float64   `json:"firstResultSeconds"`
	AcceptedSeconds    *float64   `json:"acceptedSeconds"`
	Reworks            *int       `json:"reworks"`
}

// Wait сохраняет тип и источник причины без свободного текста и ошибок.
// Ожидание зависимости не получает оценку «потеря»; интервалы разных людей параллельны.
type Wait struct {
	ActorID string    `json:"actorId"`
	Kind    string    `json:"kind"`
	Source  string    `json:"source"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Seconds float64   `json:"seconds"`
}

// interval — полуоткрытый измеренный участок, без наложения внутри одного turn.
type interval struct{ from, to time.Time }

// Build не меняет chat. Без at использует последнее сохранённое событие:
// одинаковый файл всегда даёт одинаковые числа. Явный at обрезает историю и
// позволяет измерить текущее ожидание, не подменяя неизвестный конец turn.
func Build(chat runstore.TeamChat, at time.Time) Report {
	if at.IsZero() {
		at = latest(chat)
	}
	r := Report{RunID: chat.RunID, AsOf: at, Executions: []Execution{}, Tasks: []Task{}, Waits: waits(chat, at)}
	if len(chat.Messages) > 0 {
		end := at
		if chat.Room != nil && chat.Room.AchievedAt != nil && chat.Room.AchievedAt.Before(end) {
			end = *chat.Room.AchievedAt
		}
		r.CalendarSeconds = seconds(&chat.Messages[0].Date, &end)
	}
	if chat.History != nil {
		r.WaitRecordedFrom = &chat.History.RecordedFrom
	}
	messages := map[string]runstore.TeamMessage{}
	for _, m := range chat.Messages {
		if !m.Date.After(at) {
			messages[m.ID] = m
		}
	}
	var active []interval
	complete := chat.Metrics != nil
	if chat.Metrics != nil {
		r.RecordedFrom = &chat.Metrics.RecordedFrom
		if len(chat.Messages) > 0 && chat.Messages[0].Date.Before(chat.Metrics.RecordedFrom) {
			complete = false
		}
		for _, ex := range chat.Metrics.Executions {
			if ex.ClaimedAt != nil && ex.ClaimedAt.After(at) {
				continue
			}
			snapshot := *ex
			if snapshot.StartedAt != nil && snapshot.StartedAt.After(at) {
				snapshot.StartedAt = nil
			}
			if snapshot.FinishedAt != nil && snapshot.FinishedAt.After(at) {
				snapshot.FinishedAt = nil
				snapshot.Outcome = ""
			}
			if snapshot.ObservedFinishedAt != nil && snapshot.ObservedFinishedAt.After(at) {
				snapshot.ObservedFinishedAt = nil
				if snapshot.FinishedAt == nil {
					snapshot.Outcome = ""
				}
			}
			if snapshot.Usage != nil && snapshot.Usage.At.After(at) {
				snapshot.Usage = nil
			}
			row := Execution{TeamExecution: &snapshot, Deliveries: []Delivery{}, ObservedTurnTokens: turnTokens(chat.Metrics, &snapshot)}
			for _, id := range ex.MessageIDs {
				var delay *float64
				if m, ok := messages[id]; ok && ex.StartedAt != nil && !ex.StartedAt.After(at) {
					delay = seconds(&m.Date, ex.StartedAt)
				}
				row.Deliveries = append(row.Deliveries, Delivery{MessageID: id, DelaySeconds: delay})
			}
			if ex.StartedAt != nil && ex.FinishedAt != nil && !ex.FinishedAt.After(at) && !ex.FinishedAt.Before(*ex.StartedAt) {
				pieces := []interval{{*ex.StartedAt, *ex.FinishedAt}}
				// Состояние working включает ожидание разрешения. Вычитаем только
				// наблюдаемые паузы этого сотрудника, не догадки о работе модели.
				for _, w := range r.Waits {
					if w.ActorID == ex.ActorID && w.Kind == "permission" && w.Source == "runtime" {
						pieces = subtract(pieces, interval{w.From, w.To})
					}
				}
				n := total(pieces)
				row.ActiveSeconds = &n
				r.KnownActiveAgentSeconds += n
				active = append(active, pieces...)
			} else {
				r.UnknownExecutions++
				complete = false
			}
			r.Executions = append(r.Executions, row)
		}
	}
	sort.Slice(r.Executions, func(i, j int) bool { return r.Executions[i].ID < r.Executions[j].ID })
	r.KnownActiveCalendarSeconds = unionSeconds(active)
	if complete {
		n := r.KnownActiveAgentSeconds
		r.ActiveAgentSeconds = &n
	}
	if chat.Room != nil {
		for _, task := range chat.Room.Tasks {
			m, ok := messages[task.ID]
			if !ok {
				continue
			}
			row := Task{ID: task.ID, Assignee: task.Assignee, AssignedAt: m.Date}
			reworks := 0
			for _, event := range chat.Messages {
				if event.Date.After(at) {
					continue
				}
				refers := false
				for _, id := range event.TaskIDs {
					if id == task.ID {
						refers = true
						break
					}
				}
				if !refers {
					continue
				}
				if event.Kind == "task_result" && row.FirstResultAt == nil {
					date := event.Date
					row.FirstResultAt = &date
				}
				if event.Kind == "task_rework" {
					reworks++
				}
			}
			if chat.Metrics != nil && !m.Date.Before(chat.Metrics.RecordedFrom) {
				row.Reworks = &reworks
			}
			if task.AcceptedAt != nil && !task.AcceptedAt.After(at) {
				row.AcceptedAt = task.AcceptedAt
				// Для старого принятого результата известен хотя бы его отчёт;
				// более раннюю непроверенную реплику не называем готовностью.
				if row.FirstResultAt == nil {
					if result, ok := messages[task.ResultID]; ok {
						date := result.Date
						row.FirstResultAt = &date
					}
				}
			}
			row.FirstResultSeconds = seconds(&row.AssignedAt, row.FirstResultAt)
			row.AcceptedSeconds = seconds(&row.AssignedAt, row.AcceptedAt)
			r.Tasks = append(r.Tasks, row)
		}
	}
	sort.Slice(r.Tasks, func(i, j int) bool { return r.Tasks[i].ID < r.Tasks[j].ID })
	if len(chat.Messages) > 0 {
		start := chat.Messages[0].Date
		for _, task := range r.Tasks {
			if n := seconds(&start, task.FirstResultAt); n != nil && (r.FirstResultSeconds == nil || *n < *r.FirstResultSeconds) {
				r.FirstResultSeconds = n
			}
			if n := seconds(&start, task.AcceptedAt); n != nil && (r.FirstAcceptedSeconds == nil || *n < *r.FirstAcceptedSeconds) {
				r.FirstAcceptedSeconds = n
			}
		}
	}
	return r
}

// latest определяет воспроизводимый срез без использования системных часов.
func latest(chat runstore.TeamChat) time.Time {
	var at time.Time
	take := func(t time.Time) {
		if t.After(at) {
			at = t
		}
	}
	for _, m := range chat.Messages {
		take(m.Date)
	}
	if chat.History != nil {
		for _, f := range chat.History.Frames {
			take(f.At)
		}
	}
	if chat.Metrics != nil {
		take(chat.Metrics.RecordedFrom)
		for _, e := range chat.Metrics.Executions {
			for _, t := range []*time.Time{e.ClaimedAt, e.StartedAt, e.FinishedAt, e.ObservedFinishedAt} {
				if t != nil {
					take(*t)
				}
			}
			if e.Usage != nil {
				take(e.Usage.At)
			}
		}
	}
	return at
}

// waits читает только нативные кадры: восстановленные состояния до RecordedFrom
// не доказывают точную причину. Соседние одинаковые интервалы склеиваются.
func waits(chat runstore.TeamChat, at time.Time) []Wait {
	result := []Wait{}
	if chat.History == nil {
		return result
	}
	byActor := map[string][]Wait{}
	for i, f := range chat.History.Frames {
		if f.At.Before(chat.History.RecordedFrom) || f.At.After(at) {
			continue
		}
		end := at
		if i+1 < len(chat.History.Frames) && chat.History.Frames[i+1].At.Before(end) {
			end = chat.History.Frames[i+1].At
		}
		if !end.After(f.At) {
			continue
		}
		for id, a := range f.Actors {
			if a.Wait == nil {
				continue
			}
			list := byActor[id]
			w := Wait{ActorID: id, Kind: a.Wait.Kind, Source: a.Wait.Source, From: f.At, To: end, Seconds: end.Sub(f.At).Seconds()}
			if len(list) > 0 && list[len(list)-1].To.Equal(w.From) && list[len(list)-1].Kind == w.Kind && list[len(list)-1].Source == w.Source {
				list[len(list)-1].To = end
				list[len(list)-1].Seconds += w.Seconds
			} else {
				list = append(list, w)
			}
			byActor[id] = list
		}
	}
	for _, list := range byActor {
		result = append(result, list...)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ActorID != result[j].ActorID {
			return result[i].ActorID < result[j].ActorID
		}
		return result[i].From.Before(result[j].From)
	})
	return result
}

// seconds отвергает отсутствующие и противоречивые границы, включая откат часов.
func seconds(from, to *time.Time) *float64 {
	if from == nil || to == nil || from.IsZero() || to.IsZero() || to.Before(*from) {
		return nil
	}
	n := to.Sub(*from).Seconds()
	return &n
}

// subtract вырезает наблюдаемую паузу из активных участков.
func subtract(parts []interval, cut interval) []interval {
	out := []interval{}
	for _, p := range parts {
		if !cut.to.After(p.from) || !cut.from.Before(p.to) {
			out = append(out, p)
			continue
		}
		if cut.from.After(p.from) {
			out = append(out, interval{p.from, cut.from})
		}
		if cut.to.Before(p.to) {
			out = append(out, interval{cut.to, p.to})
		}
	}
	return out
}

// total суммирует агентные интервалы; вызывающий отвечает за их непересечение.
func total(parts []interval) float64 {
	var n float64
	for _, p := range parts {
		n += p.to.Sub(p.from).Seconds()
	}
	return n
}

// unionSeconds сливает параллельные интервалы для календарной шкалы.
func unionSeconds(parts []interval) float64 {
	if len(parts) == 0 {
		return 0
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].from.Before(parts[j].from) })
	merged := []interval{parts[0]}
	for _, p := range parts[1:] {
		last := &merged[len(merged)-1]
		if !p.from.After(last.to) {
			if p.to.After(last.to) {
				last.to = p.to
			}
		} else {
			merged = append(merged, p)
		}
	}
	return total(merged)
}
