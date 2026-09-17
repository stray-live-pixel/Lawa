package teamruntime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// RecoverHistory дополняет раннюю историю существующего заказа границами turn
// из Codex. Сеть выполняется до team.lock; после чтения повторно проверяем IDs.
// Рабочая очередь, курсоры, сообщения и текущие личности остаются неизменными.
func RecoverHistory(ctx context.Context, root, run string, read func(string) ([]codex.HistoricalTurn, error)) error {
	before, err := runstore.ReadTeam(root, run)
	if err != nil {
		return err
	}
	if before.Room == nil {
		return errors.New("у заказа нет комнаты")
	}
	if before.History != nil && before.History.Recovered {
		return nil
	}
	turns := map[string][]codex.HistoricalTurn{}
	for id, actor := range before.Room.Actors {
		if actor.ThreadID == "" {
			continue
		}
		turns[id], err = read(actor.ThreadID)
		if err != nil {
			return fmt.Errorf("история %s: %w", id, err)
		}
	}
	return runstore.UpdateTeam(ctx, root, run, func(chat *runstore.TeamChat) error {
		if chat.History.Recovered {
			return nil
		}
		for id, actor := range before.Room.Actors {
			if current := chat.Room.Actors[id]; current == nil || current.ThreadID != actor.ThreadID {
				return errors.New("состав команды изменился; повторите восстановление")
			}
		}
		frames := recoveredFrames(*chat, turns, chat.History.RecordedFrom)
		chat.History.Frames = append(frames, chat.History.Frames...)
		chat.History.Recovered = true
		return nil
	})
}

// historyEvent — факт с известным временем. Для старого заказа нет детальных
// событий действий: их не выдумываем; используем только turn и реплики чата.
type historyEvent struct {
	at                     time.Time
	priority               int
	actor, status, summary string
	messages               int
}

// recoveredFrames соединяет историю двух сотрудников с append-only перепиской.
// Сохранённые нативные кадры начиная с cutoff всегда имеют приоритет. Даты turn
// имеют секундную точность: конец округляется вверх, чтобы не оказаться раньше
// последней реплики той же секунды. Промежуток без сведений помечен unknown.
func recoveredFrames(chat runstore.TeamChat, turns map[string][]codex.HistoricalTurn, cutoff time.Time) []runstore.TeamFrame {
	if len(chat.Messages) == 0 {
		return nil
	}
	// До первого нативного кадра действовала его цель, а не сегодняшняя.
	goal := chat.Goal
	if chat.History != nil && len(chat.History.Frames) > 0 && chat.History.Frames[0].Goal != "" {
		goal = chat.History.Frames[0].Goal
	}
	start := chat.Messages[0].Date
	events := []historyEvent{{at: start, actor: "boss", status: "unknown"}}
	lastDate := start
	for i, m := range chat.Messages {
		at := m.Date
		if at.Before(lastDate) {
			at = lastDate
		}
		lastDate = at
		if m.Kind == "system" && m.ID == "summon-developer" {
			events = append(events, historyEvent{at: at, actor: "developer", status: "idle"})
		}
		summary := ""
		if m.AuthorID == "boss" || m.AuthorID == "developer" {
			words := strings.Fields(strings.TrimPrefix(m.Text, "@"+m.To))
			summary = strings.Join(words[:min(7, len(words))], " ")
		}
		events = append(events, historyEvent{at: at, priority: 2, actor: m.AuthorID, summary: summary, messages: i + 1})
	}
	for id, list := range turns {
		for _, turn := range list {
			if turn.StartedAt == nil {
				continue
			}
			at := time.Unix(*turn.StartedAt, 0).UTC()
			if at.Before(start) {
				at = start
			}
			events = append(events, historyEvent{at: at, priority: 1, actor: id, status: "working", summary: "Работает над поручением"})
			if turn.CompletedAt == nil {
				continue
			}
			end := time.Unix(*turn.CompletedAt+1, 0).UTC()
			if end.Before(at) {
				continue
			}
			status, summary := "idle", ""
			if turn.Status != "completed" {
				status, summary = "blocked", "Ход остановлен"
			} else if id == "boss" {
				for _, m := range chat.Messages {
					if m.AuthorID == id && m.To == "developer" && !m.Date.Before(at) && m.Date.Before(end) {
						status = "monitoring"
					}
				}
			}
			events = append(events, historyEvent{at: end, priority: 3, actor: id, status: status, summary: summary})
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].at.Equal(events[j].at) {
			return events[i].priority < events[j].priority
		}
		return events[i].at.Before(events[j].at)
	})
	actors := map[string]runstore.TeamActorView{}
	count := 0
	var frames []runstore.TeamFrame
	for _, event := range events {
		if !event.at.Before(cutoff) {
			break
		}
		if event.messages > count {
			count = event.messages
		}
		if event.status != "" {
			actors[event.actor] = runstore.TeamActorView{Status: event.status, Summary: event.summary}
		} else if event.summary != "" {
			if actor, ok := actors[event.actor]; ok {
				actor.Summary = event.summary
				actors[event.actor] = actor
			}
		}
		frame := runstore.TeamFrame{Goal: goal, At: event.at, MessageCount: count, Actors: map[string]runstore.TeamActorView{}}
		for id, actor := range actors {
			frame.Actors[id] = actor
		}
		frames = append(frames, frame)
	}
	return frames
}
