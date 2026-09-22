package runstore

import (
	"context"
	"errors"
	"strings"
	"time"
)

// CompleteTeam — явное решение Босса, не эвристика по тексту сообщения.
// Под team.lock публикуются событие, последний ответ Челу и отметка достижения.
// Активного коллегу сначала нужно дождаться: завершение не обрывает его работу
// и не оставляет сетевой turn без владельца. Свой turn Босс завершает обычным final.
func CompleteTeam(ctx context.Context, root, run, author, id, text string) (TeamMessage, error) {
	snapshot, loadErr := TeamRoot(root, run)
	if loadErr != nil {
		return TeamMessage{}, loadErr
	}
	var result TeamMessage
	err := UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if chat.Room == nil || author != "boss" {
			return errors.New("достижение цели отмечает только Босс")
		}
		text = strings.TrimSpace(text)
		// Повтор старого финала после нового обращения только подтверждает
		// прежнюю запись, но не закрывает повторно уже возобновлённую команду.
		for i, m := range chat.Messages {
			if m.ID == id {
				if m.AuthorID == author && m.Text == text && i > 0 && chat.Messages[i-1].Kind == "achievement" {
					result = m
					return nil
				}
				return errors.New("ID уже занят сообщением")
			}
		}
		for _, m := range chat.Messages {
			if m.ID == "achievement-"+id {
				return errors.New("ID уже занят событием")
			}
		}
		if chat.Room.AchievedAt != nil {
			return errors.New("цель уже достигнута")
		}
		// Запрос обслуживания уже адресован Боссу. Достижение не должно
		// поглотить его и остановить scheduler до сохранения сводки.
		if chat.Compaction != nil && chat.Compaction.Pending != nil && chat.Compaction.Pending.Status == "pending" {
			return errors.New("сначала опубликуй запрошенную сводку прошлого через /compact")
		}
		boss := chat.Room.Actors["boss"]
		if boss == nil || boss.Delivery == nil {
			return errors.New("нет активного поручения Босса")
		}
		if TeamHasUrgentMessages(*chat, "boss", boss.Delivery.End) {
			return errors.New("сначала обработай новое обращение Чела или ответ сотрудника")
		}
		for _, m := range chat.Messages[boss.Delivery.End:] {
			if m.AuthorID == "human" && chat.Room.Actors[m.To] != nil {
				return errors.New("сначала обработай новое обращение Чела")
			}
		}
		to, err := addressedTo(text)
		if err != nil || to != "human" {
			return errors.New("итог достижения должен начинаться с @human")
		}
		for actorID, actor := range chat.Room.Actors {
			// Claim коллеги может ещё не успеть начаться. Даже если Босс уже видел
			// этот вопрос в team_read, закрытие не должно поглотить его через Cursor.
			if actorID != "boss" && TeamHasUrgentMessages(*chat, actorID, actor.Cursor) {
				return errors.New("сначала дождись ответа сотрудника на обращение Чела")
			}
			if actorID != "boss" && (actor.Delivery != nil || actor.Status == "working") {
				return errors.New("сначала дождись завершения работы сотрудников и проверь результат")
			}
		}
		for _, task := range chat.Room.Tasks {
			if task.Card != nil && task.Card.Cancellation == "cancelled" {
				continue
			}
			if task.Card != nil && len(task.Card.Results) > 0 {
				r := task.Card.Results[len(task.Card.Results)-1]
				if len(r.Artifacts) > 0 {
					if err := CheckTaskBasis(snapshot.Meta.CWD, r.Basis); err != nil {
						return err
					}
				}
			}
			if task.AcceptedAt == nil || task.Card != nil && (task.Card.Status != "done" || task.Card.Cancellation != "") {
				return errors.New("сначала проверь и прими результаты всех поручений через team_accept")
			}
		}
		// Общая маршрутизация проверяет активное поручение, лимит и авторство.
		result, err = appendRoomMessage(chat, author, id, text)
		if err != nil {
			return err
		}
		now := result.Date
		event := TeamMessage{ID: "achievement-" + id, AuthorID: "system", Kind: "achievement", Date: now, Text: "Босс отметил цель достигнутой."}
		chat.Members["system"] = TeamMember{Name: "Lawa"}
		chat.Messages[len(chat.Messages)-1] = event
		chat.Messages = append(chat.Messages, result)
		chat.Room.AchievedAt = &now
		for _, actor := range chat.Room.Actors {
			actor.NextCheck = time.Time{}
			actor.Status, actor.Summary = "idle", ""
			if actor.Delivery == nil {
				actor.Cursor = len(chat.Messages)
			}
		}
		return nil
	})
	return result, err
}
