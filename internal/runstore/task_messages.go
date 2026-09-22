package runstore

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// TeamMessageLinks — явная связь нового сообщения; старый журнал не угадываем.
type TeamMessageLinks struct {
	TaskID   string `json:"taskId,omitempty"`
	ReplyTo  string `json:"replyTo,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
}

// PostLinkedActor сохраняет сообщение один раз, применяя обычные ограничения
// адресации и обсуждений. Проверка связи предшествует изменению чата и wakeup.
func PostLinkedActor(ctx context.Context, root, run, author, id, text string, links TeamMessageLinks) (TeamMessage, error) {
	text = strings.TrimSpace(text)
	request, _ := json.Marshal(struct {
		Text  string
		Links TeamMessageLinks
	}{text, links})
	var out TeamMessage
	err := UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if chat.Room == nil {
			return errors.New("нет командной комнаты")
		}
		for _, m := range chat.Messages {
			if m.ID == id {
				if m.AuthorID != author || m.LinkInput != string(request) {
					return errors.New("ID занят другим сообщением")
				}
				out = m
				return nil
			}
		}
		if links.TaskID != "" {
			task := chat.Room.Tasks[links.TaskID]
			if task == nil {
				return errors.New("неизвестная задача этого заказа")
			}
			// Коллега может отвечать в обсуждении чужой задачи по полученной ссылке,
			// но не прикреплять произвольный комментарий к чужому поручению.
			if author != "boss" && author != "human" && task.Assignee != author && links.ReplyTo == "" {
				return errors.New("для чужой задачи ответь на адресованное тебе сообщение")
			}
			if links.Revision != 0 && (task.Card == nil || task.Card.Revision != links.Revision) {
				return errors.New("устаревшая revision сообщения")
			}
		} else if links.Revision != 0 {
			return errors.New("revision требует taskId")
		}
		if links.ReplyTo != "" {
			found := false
			for _, m := range chat.Messages {
				if m.ID == links.ReplyTo {
					found = true
					if links.TaskID != "" && m.TaskID != links.TaskID {
						return errors.New("ответ относится к другой задаче")
					}
					if links.TaskID != "" && author != "boss" && author != "human" && chat.Room.Tasks[links.TaskID].Assignee != author && m.To != author {
						return errors.New("чужой адресат сообщения задачи")
					}
				}
			}
			if !found {
				return errors.New("исходного сообщения нет в этом заказе")
			}
		}
		var err error
		input := text
		// Сохраняем счётчик ограниченных peer-обсуждений при явном ответе.
		actor := chat.Room.Actors[author]
		if author != "human" && author != "boss" && links.ReplyTo != "" && actor != nil && actor.Delivery != nil && slices.Contains(actor.Delivery.IDs, links.ReplyTo) {
			input = fmt.Sprintf("/reply %s %s", links.ReplyTo, text)
		}
		out, err = appendRoomMessage(chat, author, id, input)
		if err != nil {
			return err
		}
		out.LinkInput = string(request)
		out.TaskID, out.TaskRevision = links.TaskID, links.Revision
		if links.ReplyTo != "" {
			out.ReplyTo = links.ReplyTo
			out.ReplyToIDs = []string{links.ReplyTo}
		}
		// Маршрутизация может дописать эскалацию обсуждения после сообщения.
		// Обновляем оригинал по ID, не затирая последующее системное событие.
		for i := range chat.Messages {
			if chat.Messages[i].ID == out.ID {
				chat.Messages[i] = out
				break
			}
		}
		return nil
	})
	return out, err
}
