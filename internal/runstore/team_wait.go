package runstore

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// TeamWait объясняет ожидание, не меняя статус исполнения. Source отличает
// наблюдение runtime от заявления сотрудника; MessageID связывает его с чатом.
// Since — начало именно этой причины, а не время последнего чтения.
type TeamWait struct {
	Kind      string    `json:"kind"`
	Source    string    `json:"source"`
	Since     time.Time `json:"since"`
	ActorID   string    `json:"actorId,omitempty"`
	MessageID string    `json:"messageId,omitempty"`
	Text      string    `json:"text,omitempty"`
}

// RefreshTeamWaits вызывается внутри транзакции перед кадром. GET не обновляет
// часы и не запускает модель; legacy-комнаты без wait остаются без точной даты.
// Новая доставка сбрасывает заявление: сотрудник должен подтвердить актуальный
// блокер заново, а не оставлять его после получения новых входных данных.
func RefreshTeamWaits(chat *TeamChat, now time.Time) {
	if chat.Room == nil {
		return
	}
	for id, a := range chat.Room.Actors {
		// Повреждённые legacy-данные отклонит validateRoom при чтении.
		if a == nil || a.Cursor < 0 || a.Cursor > len(chat.Messages) {
			continue
		}
		if a.Dependency != nil && dependencyResolved(chat, id, a.Dependency) {
			a.Dependency = nil
		}
		var next *TeamWait
		switch {
		case chat.Room.AchievedAt != nil:
			a.Dependency = nil
		case a.Status == "blocked":
			kind := "error"
			if a.ApprovalPending {
				kind = "permission"
			}
			next = &TeamWait{Kind: kind, Source: "runtime", Text: a.Error}
		case a.Delivery != nil:
			if a.ApprovalPending {
				next = &TeamWait{Kind: "permission", Source: "runtime"}
			}
		case TeamHasPendingMessages(*chat, id, a.Cursor):
			kind := "queue"
			if a.CapacityPending {
				kind = "capacity"
			}
			next = &TeamWait{Kind: kind, Source: "runtime"}
		case a.Dependency != nil:
			copy := *a.Dependency
			next = &copy
		default:
			next = &TeamWait{Kind: "no_messages", Source: "runtime"}
		}
		if next != nil {
			if sameWaitReason(a.Wait, next) {
				next.Since = a.Wait.Since
			}
			if next.Since.IsZero() {
				next.Since = now.UTC()
			}
		}
		a.Wait = next
	}
}

// sameWaitReason не учитывает часы: неизменная причина сохраняет длительность.
func sameWaitReason(a, b *TeamWait) bool {
	return a != nil && b != nil && a.Kind == b.Kind && a.Source == b.Source && a.ActorID == b.ActorID && a.MessageID == b.MessageID && a.Text == b.Text
}

// dependencyResolved опирается на факты, не на смысл текста: ответ адресату
// после указанного сообщения либо явная приёмка этого результата Боссом.
func dependencyResolved(chat *TeamChat, id string, wait *TeamWait) bool {
	if wait.Kind == "acceptance" {
		found, pending := false, false
		for _, task := range chat.Room.Tasks {
			if task.Assignee == id {
				found = true
				pending = pending || task.AcceptedAt == nil
			}
		}
		// Босс мог принять те же поручения по более позднему отчёту.
		if found && !pending {
			return true
		}
	}
	after := false
	for _, m := range chat.Messages {
		if wait.Kind == "acceptance" && m.Kind == "task_accepted" && m.ResultID == wait.MessageID {
			return true
		}
		if after && wait.Kind != "acceptance" && m.AuthorID == wait.ActorID && TeamMessageForActor(m, id) {
			return true
		}
		if m.ID == wait.MessageID {
			after = true
		}
	}
	return false
}

// SetTeamWait принимает только собственное заявление активного сотрудника.
// Пустой kind снимает его. Ссылка обязательна: произвольный текст не превращает
// отсутствие сообщений в доказанное ожидание коллеги или приёмки.
func SetTeamWait(ctx context.Context, root, run, author, kind, actorID, messageID, text string) error {
	return UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if chat.Room == nil || chat.Room.AchievedAt != nil {
			return errors.New("нет активной комнаты")
		}
		a := chat.Room.Actors[author]
		if a == nil || a.Status != "working" || a.Delivery == nil {
			return errors.New("нет активного поручения")
		}
		if kind == "" {
			a.Dependency = nil
			return nil
		}
		if kind != "result" && kind != "acceptance" && kind != "permission" {
			return errors.New("причина: result, acceptance или permission")
		}
		text = strings.TrimSpace(text)
		if text == "" || len(strings.Fields(text)) > 40 || len(text) > 4000 || !utf8.ValidString(text) {
			return errors.New("объяснение: 1–40 слов, до 4 КБ")
		}
		if actorID == author || (actorID != "human" && chat.Room.Actors[actorID] == nil) {
			return errors.New("нужен другой участник ожидания")
		}
		var ref *TeamMessage
		for i := range chat.Messages {
			if chat.Messages[i].ID == messageID {
				ref = &chat.Messages[i]
				break
			}
		}
		if ref == nil || ref.Suppressed || ref.AuthorID != author || ref.To != actorID {
			return errors.New("нужно своё адресное сообщение участнику ожидания")
		}
		if kind == "acceptance" {
			if actorID != "boss" {
				return errors.New("приёмку выполняет Босс")
			}
			pending := false
			for _, task := range chat.Room.Tasks {
				if task.Assignee == author && task.AcceptedAt == nil {
					pending = true
				}
			}
			if !pending {
				return errors.New("нет непринятых поручений")
			}
		}
		next := &TeamWait{Kind: kind, Source: "actor", ActorID: actorID, MessageID: messageID, Text: text, Since: time.Now().UTC()}
		if sameWaitReason(a.Dependency, next) {
			next.Since = a.Dependency.Since
		}
		a.Dependency = next
		return nil
	})
}
