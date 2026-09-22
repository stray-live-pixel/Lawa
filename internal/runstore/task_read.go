package runstore

import (
	"errors"
	"slices"
	"strings"
)

// TaskReadOptions задаёт отдельную страницу карточки, истории или обсуждения.
// Offset относится к выбранному разделу; сообщения включают архивные оригиналы.
type TaskReadOptions struct {
	TaskID  string `json:"taskId,omitempty"`
	Section string `json:"section,omitempty"`
	Offset  int    `json:"offset,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	After   string `json:"after,omitempty"`
}

// TaskReadPage ограничивает объём ответа. NextOffset/After — явное продолжение;
// карточка не содержит всю историю и все прежние результаты одновременно.
type TaskReadPage struct {
	Task       *TeamTask  `json:"task,omitempty"`
	Tasks      []TeamTask `json:"tasks,omitempty"`
	Items      []any      `json:"items,omitempty"`
	NextOffset *int       `json:"nextOffset,omitempty"`
	After      string     `json:"after,omitempty"`
	Notice     string     `json:"notice,omitempty"`
}

// compactTask сохраняет текущие требования, а историю и доказательства отдаёт
// отдельными страницами. Полный текст текущего результата доступен в results.
func compactTask(task TeamTask) TeamTask {
	t := cloneTask(task)
	if t.Card != nil {
		t.Card.History = nil
		t.Card.Results = nil
		t.Card.Reviews = nil
	}
	return t
}

// ReadTasks не записывает team.json и не запускает модель; проверка файлов лишь
// уточняет видимое текущее состояние, не стирая историческую приёмку.
func ReadTasks(root, run string, o TaskReadOptions) (TaskReadPage, error) {
	out := TaskReadPage{}
	if o.Offset < 0 || o.Limit < 0 || o.Limit > 100 {
		return out, errors.New("неверные границы страницы")
	}
	if o.Limit == 0 {
		o.Limit = 20
	}
	chat, err := ReadTeam(root, run)
	if err != nil {
		return out, err
	}
	if o.TaskID == "" {
		list, next, err := TaskPage(chat, o.After, o.Limit)
		if err != nil {
			return out, err
		}
		size := 0
		for i, t := range list {
			summary := compactTask(t)
			summary.Text = ""
			if summary.Card != nil {
				summary.Card.Expected = ""
				summary.Card.Criteria = nil
			}
			cost := EstimateTeamTokens(summary)
			if size+cost > 8000 && i > 0 {
				out.After = list[i-1].ID
				break
			}
			out.Tasks = append(out.Tasks, summary)
			size += cost
		}
		if out.After == "" {
			out.After = next
		}
		out.Notice = "Требования: taskId. Разделы: history, results, reviews, messages. Старые поручения не имеют проверенной revision."
		return out, nil
	}
	if chat.Room == nil || chat.Room.Tasks[o.TaskID] == nil {
		return out, errors.New("задача не найдена")
	}
	t := chat.Room.Tasks[o.TaskID]
	if o.Section == "" || o.Section == "card" {
		task := compactTask(*t)
		if task.Card != nil && len(t.Card.Results) > 0 {
			r := t.Card.Results[len(t.Card.Results)-1]
			if r.Revision != t.Card.Revision {
				task.Card.StaleReason = "Результат относится к старой revision"
			} else if len(r.Artifacts) > 0 {
				snapshot, err := TeamRoot(root, run)
				if err != nil {
					return out, err
				}
				if err = CheckTaskBasis(snapshot.Meta.CWD, r.Basis); err != nil {
					task.Card.StaleReason = err.Error()
					if task.Card.Status == "done" {
						task.Card.Status = "in_review"
					}
				}
			}
		}
		out.Task = &task
		out.Notice = "Полные доказательства и история читаются по разделам results, reviews, history, messages."
		return out, nil
	}
	var items []any
	switch o.Section {
	case "history":
		if t.Card != nil {
			for _, v := range t.Card.History {
				items = append(items, v)
			}
		}
	case "results":
		if t.Card != nil {
			for _, v := range t.Card.Results {
				items = append(items, v)
			}
		}
	case "reviews":
		if t.Card != nil {
			for _, v := range t.Card.Reviews {
				items = append(items, v)
			}
		}
	case "messages":
		for _, v := range chat.Messages {
			if v.TaskID == o.TaskID || slices.Contains(v.TaskIDs, o.TaskID) {
				items = append(items, v)
			}
		}
	default:
		return out, errors.New("раздел: card, history, results, reviews или messages")
	}
	if o.Offset > len(items) {
		return out, errors.New("позиция вне раздела")
	}
	size := 0
	for i := o.Offset; i < len(items); i++ {
		cost := EstimateTeamTokens(items[i])
		if len(out.Items) >= o.Limit || len(out.Items) > 0 && size+cost > 24000 {
			next := i
			out.NextOffset = &next
			break
		}
		out.Items = append(out.Items, items[i])
		size += cost
	}
	return out, nil
}

// TaskCommandText распознаёт fallback для thread с закреплённой старой schema.
func TaskCommandText(text string) (string, string, bool) {
	for _, prefix := range []string{"/task ", "/task_read "} {
		if value, ok := strings.CutPrefix(text, prefix); ok {
			return strings.TrimSpace(prefix), value, true
		}
	}
	return "", "", false
}
