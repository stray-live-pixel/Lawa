package runstore

import (
	"fmt"
	"sort"
	"time"
)

// taskReady требует принятой актуальной зависимости. Окончание чужого turn,
// отмена и старое AcceptedAt не заменяют текущий статус карточки.
func taskReady(chat *TeamChat, t *TeamTask) bool {
	c := t.Card
	if c == nil || t.Assignee == "" || c.Status != "todo" || c.Blocker != "" || c.Cancellation != "" || c.ScheduleKey == c.DispatchedKey {
		return false
	}
	for _, id := range c.Dependencies {
		d := chat.Room.Tasks[id]
		if d == nil || d.Card == nil || d.Card.Status != "done" || d.Card.Cancellation != "" || d.Card.StaleReason != "" {
			return false
		}
	}
	return true
}

// wakeReadyTasks выставляет срок только свободным личностям; активную доставку
// не меняет. Приёмка предшественника и снятие блокера используют тот же путь.
func wakeReadyTasks(chat *TeamChat) {
	for _, t := range chat.Room.Tasks {
		if taskReady(chat, t) {
			if a := chat.Room.Actors[t.Assignee]; a != nil && a.Delivery == nil && a.Status != "blocked" {
				a.NextCheck = time.Now().UTC()
			}
		}
	}
}

// selectTaskDispatch атомарно выбирает карточку по приоритету, затем ID.
// Доставка ссылается на исходное событие операции: второго сообщения нет.
func selectTaskDispatch(chat *TeamChat, actorID string) *TeamTask {
	for _, t := range chat.Room.Tasks {
		if t.Assignee == actorID && t.Card != nil && t.Card.Status == "in_progress" && t.Card.Cancellation == "" && t.Card.Blocker == "" {
			return nil
		}
	}
	candidates := []*TeamTask{}
	for _, t := range chat.Room.Tasks {
		if t.Assignee == actorID && taskReady(chat, t) {
			candidates = append(candidates, t)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Card.Priority != b.Card.Priority {
			return a.Card.Priority > b.Card.Priority
		}
		return a.ID < b.ID
	})
	if len(candidates) == 0 {
		return nil
	}
	t := candidates[0]
	// Между свободными личностями приоритет тоже проверяется под общим lock.
	// Если более приоритетная выдача ещё не зарезервирована, текущий Process
	// освобождает capacity; следующий тик даст слот первому кандидату.
	for _, other := range chat.Room.Tasks {
		if other.Assignee == actorID || !taskReady(chat, other) {
			continue
		}
		a := chat.Room.Actors[other.Assignee]
		if a == nil || a.Delivery != nil || a.Status == "blocked" {
			continue
		}
		busy := false
		for _, active := range chat.Room.Tasks {
			if active.Assignee == other.Assignee && active.Card != nil && active.Card.Status == "in_progress" && active.Card.Cancellation == "" && active.Card.Blocker == "" {
				busy = true
			}
		}
		if busy {
			continue
		}
		if other.Card.Priority > t.Card.Priority || other.Card.Priority == t.Card.Priority && other.ID < t.ID {
			return nil
		}
	}
	t.Card.DispatchedKey = t.Card.ScheduleKey
	return t
}

// invalidateTaskDependents сохраняет историческую приёмку и требует новую работу
// при изменении принятой зависимости. Обход конечен благодаря seen и запрету циклов.
func invalidateTaskDependents(chat *TeamChat, source string) {
	seen := map[string]bool{source: true}
	var visit func(string)
	visit = func(id string) {
		for _, t := range chat.Room.Tasks {
			if seen[t.ID] || t.Card == nil {
				continue
			}
			depends := false
			for _, dep := range t.Card.Dependencies {
				if dep == id {
					depends = true
				}
			}
			if !depends {
				continue
			}
			seen[t.ID] = true
			c := t.Card
			c.StaleReason = "Изменилась или отменена зависимость " + id
			c.Revision++
			c.Version++
			c.Acknowledged = 0
			if c.Cancellation == "" {
				c.Status = "todo"
			}
			key := fmt.Sprintf("dependency-%s-%d", t.ID, c.Version)
			c.ScheduleKey = key
			change := TaskChange{ID: key, Author: "system", Action: "dependency_stale", Date: time.Now().UTC(), FromRevision: c.Revision - 1, Revision: c.Revision, Reason: c.StaleReason}
			c.History = append(c.History, change)
			m := TeamMessage{ID: key, AuthorID: "system", To: t.Assignee, Kind: "task_change", TaskID: t.ID, TaskRevision: c.Revision, Text: c.StaleReason, Date: change.Date}
			snapshot := compactTask(*t)
			m.TaskSnapshot = &snapshot
			chat.Messages = append(chat.Messages, m)
			wakeForMessage(chat, m)
			visit(t.ID)
		}
	}
	visit(source)
}
