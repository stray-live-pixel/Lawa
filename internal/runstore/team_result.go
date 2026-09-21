package runstore

import (
	"errors"
	"strings"
)

// postResultEvent делает результат и возврат явными рабочими сообщениями.
// Это не оценка текста моделью: проверяемость заявляет сотрудник, приёмку — Босс.
// Обычная маршрутизация сохраняет адресность, пробуждение и связи обсуждения.
func postResultEvent(chat *TeamChat, author, id, input string) (TeamMessage, error) {
	for _, m := range chat.Messages {
		if m.ID == id {
			if m.AuthorID == author && m.DiscussionInput == input {
				return m, nil
			}
			return TeamMessage{}, errors.New("ID занят другим сообщением")
		}
	}
	command, rest, _ := strings.Cut(input, " ")
	var taskIDs []string
	resultID := ""
	if command == "/result" {
		a := chat.Room.Actors[author]
		if author == "boss" || author == "human" || a == nil || a.Delivery == nil || !strings.HasPrefix(rest, "@boss ") {
			return TeamMessage{}, errors.New("результат сотрудника: /result @boss <что готово, где проверить>")
		}
		for i, m := range chat.Messages {
			task := chat.Room.Tasks[m.ID]
			if i < a.Delivery.End && task != nil && task.Assignee == author && task.AcceptedAt == nil {
				taskIDs = append(taskIDs, task.ID)
			}
		}
		if len(taskIDs) == 0 {
			return TeamMessage{}, errors.New("нет полученных непринятых поручений")
		}
	} else {
		resultID, rest, _ = strings.Cut(rest, " ")
		var report *TeamMessage
		for i := range chat.Messages {
			if chat.Messages[i].ID == resultID {
				report = &chat.Messages[i]
				break
			}
		}
		if author != "boss" || report == nil || report.Kind != "task_result" || !strings.HasPrefix(rest, "@"+report.AuthorID+" ") {
			return TeamMessage{}, errors.New("возврат Босса: /rework <ID результата> @сотрудник <что исправить>")
		}
		for _, taskID := range report.TaskIDs {
			task := chat.Room.Tasks[taskID]
			if task != nil && task.Assignee == report.AuthorID && task.AcceptedAt == nil {
				taskIDs = append(taskIDs, taskID)
			}
		}
		if len(taskIDs) == 0 {
			return TeamMessage{}, errors.New("поручения результата уже приняты")
		}
	}
	m, err := appendRoomMessage(chat, author, id, rest)
	if err != nil {
		return m, err
	}
	m.Kind = "task_result"
	if command == "/rework" {
		m.Kind = "task_rework"
		// Возврат продолжает прежнее обязательство, не создаёт дубликат поручения.
		delete(chat.Room.Tasks, m.ID)
	}
	m.TaskIDs, m.ResultID, m.DiscussionInput = taskIDs, resultID, input
	chat.Messages[len(chat.Messages)-1] = m
	return m, nil
}
