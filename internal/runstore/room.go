package runstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

const TeamIdleInterval = 5 * time.Minute

// TeamRoom включает адресный runtime только для новых командных заказов.
// Старые фиксированные workflow и их общая база не меняют способ исполнения.
type TeamRoom struct {
	// Catalog — снимок доступных личностей из workflow при создании заказа.
	// Только Actors означает приглашённых сотрудников. Каталог не меняется в ходе заказа.
	Catalog map[string]workflow.Character `json:"catalog,omitempty"`
	// AchievedAt останавливает новые поручения; nil сохраняет прежний активный режим.
	AchievedAt *time.Time            `json:"achievedAt,omitempty"`
	Actors     map[string]*TeamActor `json:"actors"`
}

// TeamActor — личность на весь заказ, с одним Codex thread и последовательными
// turn. Cursor — число просмотренных сообщений; во время работы не сдвигается.
type TeamActor struct {
	ThreadID  string        `json:"threadId,omitempty"`
	TurnID    string        `json:"turnId,omitempty"`
	Cursor    int           `json:"cursor"`
	NextCheck time.Time     `json:"nextCheck"`
	Status    string        `json:"status"`
	Summary   string        `json:"summary,omitempty"`
	Error     string        `json:"error,omitempty"`
	Delivery  *TeamDelivery `json:"delivery,omitempty"`
}

// TeamDelivery фиксируется до сети. End отделяет текущую порцию адресных
// сообщений от поступивших во время работы. Attempted запрещает слепой повтор.
type TeamDelivery struct {
	TurnID    string   `json:"turnId,omitempty"` // Только текущая доставка, не предыдущий turn личности.
	IDs       []string `json:"ids"`
	End       int      `json:"end"`
	Attempted bool     `json:"attempted"`
}

// initializeRoom создаёт только Босса. Постановка цели является первым явным
// обращением Чела; полный текст остаётся в pin, краткая реплика запускает анализ.
func initializeRoom(chat *TeamChat, catalog map[string]workflow.Character) {
	now := time.Now().UTC()
	chat.Members["boss"] = TeamMember{Name: catalog["boss"].Name, Avatar: catalog["boss"].Avatar}
	chat.Room = &TeamRoom{Catalog: catalog, Actors: map[string]*TeamActor{"boss": {NextCheck: now, Status: "idle"}}}
	chat.Messages = append(chat.Messages, TeamMessage{ID: "initial-goal", AuthorID: "human", To: "boss", Kind: "goal", Date: now, Text: "@boss Проанализируй закреплённую цель и организуй выполнение."})
	recordTeamFrame(chat, now)
}

// addressedTo распознаёт ровно один явный адрес в начале сообщения. Вложения,
// цитаты и случайные упоминания внутри текста не запускают чужие turn.
func addressedTo(text string) (string, error) {
	words := strings.Fields(text)
	if len(words) == 0 || !strings.HasPrefix(words[0], "@") {
		return "", nil
	}
	id := strings.TrimPrefix(words[0], "@")
	if !workflow.ValidTeamID(id) && id != "human" {
		return "", errors.New("используйте @id сотрудника в начале сообщения")
	}
	if len(words) < 2 {
		return "", errors.New("после @id нужен текст поручения")
	}
	return id, nil
}

// appendRoomMessage — единая маршрутизация под team.lock. Сотрудники пишут
// только во время адресного поручения; их ответы ссылаются на входящее сообщение.
// Единственные исключения: Босс делегирует/уточняет, сотрудник передаёт ответ
// Челу через Босса. Только Босс имеет право адресовать сообщение человеку.
func appendRoomMessage(chat *TeamChat, author, id, text string) (TeamMessage, error) {
	text = strings.TrimSpace(text)
	if len(strings.Fields(text)) < 1 || len(strings.Fields(text)) > 50 || len(text) > 8192 || !utf8.ValidString(text) || id == "" || len(id) > 200 {
		return TeamMessage{}, errors.New("сообщение: 1–50 слов, до 8 КБ, непустой ID")
	}
	for _, previous := range chat.Messages {
		if previous.ID == id {
			if previous.AuthorID != author || previous.Text != text {
				return TeamMessage{}, errors.New("ID занят другим сообщением")
			}
			return previous, nil
		}
	}
	if chat.Room.AchievedAt != nil && author != "human" {
		return TeamMessage{}, errors.New("цель достигнута; дождись нового обращения Чела")
	}
	to, err := addressedTo(text)
	if err != nil {
		return TeamMessage{}, err
	}
	if to != "" && to != "human" && chat.Room.Actors[to] == nil {
		return TeamMessage{}, errors.New("сначала Босс должен пригласить сотрудника")
	}
	m := TeamMessage{ID: id, AuthorID: author, To: to, Text: text, Date: time.Now().UTC(), Kind: "message"}
	if author != "human" {
		actor := chat.Room.Actors[author]
		if actor == nil || actor.Delivery == nil || actor.Status != "working" {
			return m, errors.New("нет активного адресного поручения")
		}
		if to == "" || to == author {
			return m, errors.New("ответ должен начинаться с @id получателя")
		}
		if author != "boss" && to == "human" {
			return m, errors.New("ответ Челу передай через @boss")
		}
		for _, msg := range chat.Messages {
			for _, inputID := range actor.Delivery.IDs {
				if msg.ID == inputID && (msg.AuthorID == to || author != "boss" && msg.AuthorID == "human" && to == "boss") {
					m.ReplyTo = msg.ID
					m.ReplyToIDs = append(m.ReplyToIDs, msg.ID)
				}
			}
		}
		if author == "boss" && to == "human" && pendingHumanRelay(*chat) {
			return m, errors.New("сначала дождись ответа сотрудника на обращение Чела")
		}
		if m.ReplyTo != "" {
			m.Kind = "reply"
		} else if author == "boss" && (chat.Room.Actors[to] != nil || to == "human") {
			m.Kind = "request"
		} else {
			return m, errors.New("можно отвечать только отправителю текущего поручения")
		}
		actor.Summary = strings.Join(strings.Fields(strings.TrimPrefix(text, "@"+to))[:min(7, len(strings.Fields(strings.TrimPrefix(text, "@"+to))))], " ")
	}
	reopenForHuman(chat, &m)
	chat.Messages = append(chat.Messages, m)
	wakeForMessage(chat, m)
	return m, nil
}

// PostActor не принимает авторство из аргументов модели: author захватывает
// runtime при запуске личности. Человек использует прежний PostTeam.
func PostActor(ctx context.Context, root, run, author, id, text string) (TeamMessage, error) {
	var result TeamMessage
	err := UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if chat.Room == nil {
			return errors.New("нет командной комнаты")
		}
		var err error
		result, err = appendRoomMessage(chat, author, id, text)
		return err
	})
	return result, err
}

// SummonDeveloper оставляет совместимость с прежними вызовами хранилища.
func SummonDeveloper(ctx context.Context, root, run, author string) error {
	return SummonActor(ctx, root, run, author, "developer")
}

// SummonActor атомарно добавляет рабочее место и событие из сохранённого каталога.
// Повтор идемпотентен; приглашение само по себе не запускает поручение.
func SummonActor(ctx context.Context, root, run, author, id string) error {
	return UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if chat.Room == nil || chat.Room.AchievedAt != nil || author != "boss" || chat.Room.Actors["boss"] == nil || chat.Room.Actors["boss"].Status != "working" {
			return errors.New("пригласить сотрудника может только работающий Босс")
		}
		character, ok := chat.Room.Catalog[id]
		if !ok || id == "boss" {
			return errors.New("сотрудника нет в доступном каталоге")
		}
		if chat.Room.Actors[id] != nil {
			return nil
		}
		now := time.Now().UTC()
		chat.Members[id] = TeamMember{Name: character.Name, Avatar: character.Avatar}
		chat.Members["system"] = TeamMember{Name: "Lawa"}
		chat.Room.Actors[id] = &TeamActor{Cursor: len(chat.Messages), NextCheck: now.Add(TeamIdleInterval), Status: "idle"}
		chat.Messages = append(chat.Messages, TeamMessage{ID: "summon-" + id, AuthorID: "system", Kind: "system", Date: now, Text: fmt.Sprintf("%s пригласил сотрудника %s (@%s). Рабочее место готово.", chat.Members["boss"].Name, character.Name, id)})
		return nil
	})
}

// ClaimTeamDelivery проверяет личный таймер и резервирует только явные теги.
// Пустая проверка не запускает LLM. Вызывать под долгим LockTeamActor.
func ClaimTeamDelivery(ctx context.Context, root, run, actorID string, now time.Time) (bool, error) {
	claimed := false
	err := UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if chat.Room == nil {
			return errors.New("нет комнаты")
		}
		if chat.Room.AchievedAt != nil {
			return nil
		}
		actor := chat.Room.Actors[actorID]
		if actor == nil || TeamActorSleeping(actor) || actor.Delivery != nil || actor.Status == "blocked" || now.Before(actor.NextCheck) {
			return nil
		}
		if actor.Cursor < 0 || actor.Cursor > len(chat.Messages) {
			return errors.New("повреждён курсор истории")
		}
		var ids []string
		for i := actor.Cursor; i < len(chat.Messages); i++ {
			if TeamMessageForActor(chat.Messages[i], actorID) {
				ids = append(ids, chat.Messages[i].ID)
			}
		}
		if len(ids) == 0 {
			actor.Cursor = len(chat.Messages)
			actor.NextCheck = now.Add(TeamIdleInterval)
			return nil
		}
		actor.Delivery = &TeamDelivery{IDs: ids, End: len(chat.Messages)}
		actor.Status, actor.Summary, actor.Error = "working", "Читает поручение", ""
		claimed = true
		return nil
	})
	return claimed, err
}

// LockTeamActor удерживает отдельный flock весь turn. Второй сервер не запускает
// ту же личность; после аварии ОС освобождает lock для проверки сохранённой доставки.
func LockTeamActor(root, run, actor string) (*os.File, error) {
	if !workflow.ValidTeamID(actor) {
		return nil, errors.New("неизвестная личность")
	}
	dir, err := openRun(root, run)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	name := "team-" + actor + ".lock"
	if info, e := dir.Lstat(name); e == nil && !info.Mode().IsRegular() {
		return nil, errors.New("lock должен быть обычным файлом")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	f, err := dir.OpenFile(name, os.O_CREATE|os.O_RDWR|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("необычный lock")
	}
	if err == nil {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("личность занята: %w", err)
	}
	return f, nil
}

// validateRoom отклоняет повреждённый курсор/сотрудника до обхода Engine.
// Ошибка одного файла показывается в API, а не обрушает весь сервер panic-ом.
func (chat TeamChat) validateRoom() error {
	if err := workflow.ValidateTeamCharacters(chat.Room.Catalog); err != nil {
		return err
	}
	if chat.Room.Actors["boss"] == nil {
		return errors.New("в комнате отсутствует Босс")
	}
	// После достижения Чел может дописывать заметки. Ищем соответствующий
	// финал во всей истории, а не считаем последнюю реплику неизменной.
	if at := chat.Room.AchievedAt; at != nil {
		found := false
		for i, m := range chat.Messages {
			if m.Kind == "achievement" && m.Date.Equal(*at) && i+1 < len(chat.Messages) && chat.Messages[i+1].AuthorID == "boss" && chat.Messages[i+1].To == "human" {
				found = true
			}
		}
		if at.IsZero() || !found {
			return errors.New("повреждено завершение цели")
		}
	}
	for id, actor := range chat.Room.Actors {
		if (!workflow.ValidTeamID(id) || chat.Room.Catalog[id].Name == "") || actor == nil || actor.Cursor < 0 || actor.Cursor > len(chat.Messages) {
			return fmt.Errorf("повреждён сотрудник %q", id)
		}
		switch actor.Status {
		case "idle", "working", "monitoring", "blocked":
		default:
			return fmt.Errorf("неизвестный статус сотрудника %q", id)
		}
		if d := actor.Delivery; d != nil && (d.End < actor.Cursor || d.End > len(chat.Messages) || len(d.IDs) == 0) {
			return fmt.Errorf("повреждена доставка сотрудника %q", id)
		}
	}
	return nil
}
