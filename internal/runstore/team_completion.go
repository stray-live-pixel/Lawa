package runstore

import (
	"context"
	"errors"
	"strings"
	"time"
)

// CompleteTeam — явное решение Босса, не эвристика по тексту сообщения.
// Под team.lock публикуются событие, последний ответ Челу и терминальная отметка.
// Активного коллегу сначала нужно дождаться: завершение не обрывает его работу
// и не оставляет сетевой turn без владельца. Свой turn Босс завершает обычным final.
func CompleteTeam(ctx context.Context, root, run, author, id, text string) (TeamMessage, error) {
	var result TeamMessage
	err := UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if chat.Room == nil || author != "boss" {
			return errors.New("достижение цели отмечает только Босс")
		}
		text = strings.TrimSpace(text)
		if chat.Room.AchievedAt != nil {
			// Потерянный ответ tool-call не должен дублировать финал даже после рестарта.
			last := chat.Messages[len(chat.Messages)-1]
			if last.ID == id && last.AuthorID == author && last.Text == text {
				result = last
				return nil
			}
			return errors.New("цель уже достигнута")
		}
		for _, m := range chat.Messages {
			if m.ID == id || m.ID == "achievement-"+id {
				return errors.New("ID уже занят сообщением")
			}
		}
		to, err := addressedTo(text)
		if err != nil || to != "human" {
			return errors.New("итог достижения должен начинаться с @human")
		}
		for actorID, actor := range chat.Room.Actors {
			if actorID != "boss" && (actor.Delivery != nil || actor.Status == "working") {
				return errors.New("сначала дождись завершения работы сотрудников и проверь результат")
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
