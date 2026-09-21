package runstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// TeamTask — обязательство перед командой. ID совпадает с исходным сообщением:
// получение и завершение Codex turn не означают выполнения поручения. Только
// Босс после проверки конкретного результата записывает AcceptedAt и Evidence.
// Любое обращение Босса/Чела к сотруднику учитывается, включая уточнения.
// Цель самого Босса хранится отдельно в pin и завершается через CompleteTeam.
type TeamTask struct {
	ID         string     `json:"id"`
	Assignee   string     `json:"assignee"`
	Text       string     `json:"text"`
	ResultID   string     `json:"resultId,omitempty"`
	Evidence   string     `json:"evidence,omitempty"`
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
}

// initializeTeamTasks восстанавливает обязательства старой активной комнаты
// без записи на GET. До последнего достижения история уже завершена: нельзя
// задним числом требовать её повторного исполнения. Незавершённые поручения
// активного периода остаются открытыми даже при наличии ответа сотрудника.
func initializeTeamTasks(chat *TeamChat) {
	if chat.Room.Tasks != nil {
		return
	}
	chat.Room.Tasks = map[string]*TeamTask{}
	start := 0
	for i, m := range chat.Messages {
		if m.Kind == "achievement" {
			start = i + 1
		}
	}
	for _, m := range chat.Messages[start:] {
		recordTeamTask(chat, m)
	}
}

// recordTeamTask вызывается только после добавления уникального сообщения под
// team.lock. Модель не управляет созданием обязательства отдельным инструментом
// и не может забыть зарегистрировать выданное в чате поручение.
func recordTeamTask(chat *TeamChat, m TeamMessage) {
	if chat.Room.Tasks == nil {
		chat.Room.Tasks = map[string]*TeamTask{}
	}
	if m.Kind != "discussion_decision" && m.Kind != "task_rework" && (m.AuthorID == "boss" || m.AuthorID == "human") && m.To != "boss" && chat.Room.Actors[m.To] != nil {
		chat.Room.Tasks[m.ID] = &TeamTask{ID: m.ID, Assignee: m.To, Text: m.Text}
	}
}

// AcceptTeamTasks атомарно принимает выбранные обязательства одного сотрудника.
// Босс указывает сообщение с результатом и описание собственной проверки.
// Сервер проверяет происхождение и порядок сообщений, доставку и завершение
// хода, но смысловую полноту результата обязан оценить Босс. Один поздний отчёт
// может закрывать несколько поручений, в том числе повторный запрос отчёта
// после молчаливого turn; поэтому связи replyTo не являются единственным ключом.
func AcceptTeamTasks(ctx context.Context, root, run, author, id string, ids []string, resultID, evidence string) (TeamMessage, error) {
	var result TeamMessage
	err := UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if chat.Room == nil || author != "boss" {
			return errors.New("результаты принимает только Босс")
		}
		evidence = strings.TrimSpace(evidence)
		if len(ids) == 0 || len(ids) > 100 || evidence == "" || len(strings.Fields(evidence)) > 40 || len(evidence) > 6000 || !utf8.ValidString(evidence) || id == "" {
			return errors.New("нужны ID поручений, ID результата и описание проверки до 40 слов")
		}
		for _, m := range chat.Messages {
			if m.ID == id {
				if m.Kind == "task_accepted" && slices.Equal(m.TaskIDs, ids) && m.ResultID == resultID && m.Text == "Босс принял результат. "+evidence {
					result = m
					return nil
				}
				return errors.New("ID занят другим событием")
			}
		}
		boss := chat.Room.Actors["boss"]
		if chat.Room.AchievedAt != nil || boss == nil || boss.Delivery == nil || boss.Status != "working" {
			return errors.New("нет активного поручения Босса")
		}
		resultIndex := -1
		var report TeamMessage
		positions := map[string]int{}
		for i, m := range chat.Messages {
			positions[m.ID] = i
			if m.ID == resultID {
				report = m
				resultIndex = i
			}
		}
		actor := chat.Room.Actors[report.AuthorID]
		if resultIndex < 0 || report.To != "boss" || report.AuthorID == "boss" || actor == nil {
			return errors.New("нужно сообщение сотрудника Боссу с результатом")
		}
		if actor.Delivery != nil || actor.Status == "working" || actor.Status == "blocked" {
			return errors.New("сначала дождись успешного завершения хода сотрудника")
		}
		seen := map[string]bool{}
		for _, taskID := range ids {
			task := chat.Room.Tasks[taskID]
			if task == nil || task.Assignee != report.AuthorID || seen[taskID] {
				return errors.New("неизвестное, чужое или повторное поручение")
			}
			seen[taskID] = true
			pos, ok := positions[taskID]
			if !ok || resultIndex <= pos || actor.Cursor <= pos {
				return errors.New("результат должен следовать за полученным поручением")
			}
			if task.AcceptedAt != nil {
				return errors.New("поручение уже принято")
			}
			// Возврат продолжает исходную задачу. Без этой проверки Босс мог бы
			// принять старый отчёт и закрыть цель, пока доработка ещё в очереди.
			for i, message := range chat.Messages {
				if message.Kind == "task_rework" && slices.Contains(message.TaskIDs, taskID) && (i >= resultIndex || i >= actor.Cursor) {
					return errors.New("после возврата нужен новый результат полученной доработки")
				}
			}
		}
		now := time.Now().UTC()
		for _, taskID := range ids {
			task := chat.Room.Tasks[taskID]
			task.ResultID = resultID
			task.Evidence = evidence
			task.AcceptedAt = &now
		}
		result = TeamMessage{ID: id, AuthorID: "system", Kind: "task_accepted", Date: now, Text: "Босс принял результат. " + evidence, TaskIDs: slices.Clone(ids), ResultID: resultID}
		chat.Members["system"] = TeamMember{Name: "Lawa"}
		chat.Messages = append(chat.Messages, result)
		return nil
	})
	return result, err
}

// NotifyTeamProblem публикует адресное событие для Босса в той же транзакции,
// что и ошибка/завершение turn. Идентификатор привязан к доставке: повторное
// восстановление не создаёт бесконечную очередь одинаковых уведомлений.
// Босс получает повод действовать, а не право повторять неоднозначную доставку.
func NotifyTeamProblem(chat *TeamChat, actorID, deliveryID, problem string) {
	if chat.Room.AchievedAt != nil {
		return
	}
	id := fmt.Sprintf("problem-%s-%d-%s", actorID, chat.Room.Actors[actorID].Attempt, deliveryID)
	for _, m := range chat.Messages {
		if m.ID == id {
			return
		}
	}
	text := fmt.Sprintf("@boss Сотрудник @%s: %s. Проверь состояние и организуй продолжение; цель не достигнута.", actorID, problem)
	m := TeamMessage{ID: id, AuthorID: "system", To: "boss", Kind: "system", Date: time.Now().UTC(), Text: text}
	chat.Members["system"] = TeamMember{Name: "Lawa"}
	chat.Messages = append(chat.Messages, m)
	wakeForMessage(chat, m)
}

// NotifyTeamResultReady нужен только если Босс уже получил отчёт, пока коллега
// ещё завершал turn. Иначе исходный @boss уже готов к ближайшему циклу scheduler.
// Событие не даёт Боссу зависнуть после корректного отказа преждевременной приёмки.
func NotifyTeamResultReady(chat *TeamChat, actorID, resultID string) {
	boss := chat.Room.Actors["boss"]
	if boss == nil || chat.Room.AchievedAt != nil {
		return
	}
	index := -1
	for i, m := range chat.Messages {
		if m.ID == resultID {
			index = i
			break
		}
	}
	if index < 0 || boss.Cursor <= index && (boss.Delivery == nil || boss.Delivery.End <= index) {
		return
	}
	id := "ready-" + resultID
	for _, m := range chat.Messages {
		if m.ID == id {
			return
		}
	}
	m := TeamMessage{ID: id, AuthorID: "system", To: "boss", Kind: "system", Date: time.Now().UTC(), Text: fmt.Sprintf("@boss Сотрудник @%s завершил ход. Проверь его отчёт и прими выполненные поручения.", actorID)}
	chat.Members["system"] = TeamMember{Name: "Lawa"}
	chat.Messages = append(chat.Messages, m)
	wakeForMessage(chat, m)
}
