package runstore

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// TeamReadSchema общий для workflow и офиса. Пустой вызов возвращает снимок;
// курсор явно передаётся клиентом и не изменяет подтверждения адресной доставки.
const TeamReadSchema = `{"type":"object","properties":{"cursor":{"type":"string"},"archive":{"type":"boolean"},"from":{"type":"integer","minimum":1},"through":{"type":"integer","minimum":1},"ids":{"type":"array","items":{"type":"string"},"maxItems":100},"limit":{"type":"integer","minimum":1,"maximum":100}},"additionalProperties":false}`

// TeamReadOptions задаёт страницу событий. Диапазоны включают обе границы,
// позиции считаются от 1; архив читается только по явному archive=true.
type TeamReadOptions struct {
	Cursor  string   `json:"cursor,omitempty"`
	Archive bool     `json:"archive,omitempty"`
	From    int      `json:"from,omitempty"`
	Through int      `json:"through,omitempty"`
	IDs     []string `json:"ids,omitempty"`
	Limit   int      `json:"limit,omitempty"`
}

// TeamContext отделяет текущие обязательства от переписки. Room оставлен для
// совместимости team_read.room.tasks, но не содержит завершённой истории работы.
type TeamContext struct {
	RunID           string                 `json:"runId"`
	Goal            string                 `json:"goal"`
	Members         map[string]TeamMember  `json:"members"`
	Room            *TeamRoom              `json:"room,omitempty"`
	Summary         *TeamSummary           `json:"summary,omitempty"`
	Compaction      *TeamCompactionRequest `json:"compaction,omitempty"`
	Messages        []TeamMessage          `json:"messages"`
	Cursor          string                 `json:"cursor,omitempty"`
	HasMore         bool                   `json:"hasMore"`
	Head            int                    `json:"head"`
	ResetRequired   bool                   `json:"resetRequired,omitempty"`
	ResumeCursor    string                 `json:"resumeCursor,omitempty"`
	Notice          string                 `json:"notice,omitempty"`
	EstimatedTokens int                    `json:"estimatedTokens"`
}

// contextCursor привязан к заказу и последнему событию. Проверка ID обнаруживает
// подмену/укорочение журнала; base64 — формат транспорта, не средство авторизации.
type contextCursor struct {
	Run      string
	Position int
	ID       string
	Archive  bool
	Through  int
	Filter   string
}

// EstimateTeamTokens — воспроизводимая инженерная оценка: один токен
// на три Unicode-символа JSON. Это не точный токенизатор конкретной модели.
func EstimateTeamTokens(value any) int {
	data, _ := json.Marshal(value)
	return (utf8.RuneCount(data) + 2) / 3
}

// latestTeamSummary выводится только из опубликованных событий. Нет отдельного
// изменяемого указателя, который мог бы показать будущую сводку в старом кадре.
func latestTeamSummary(chat TeamChat) *TeamSummary {
	for i := len(chat.Messages) - 1; i >= 0; i-- {
		if chat.Messages[i].Summary != nil {
			return chat.Messages[i].Summary
		}
	}
	return nil
}

// ReadTeamContext читает единый атомарный снимок, затем ограничивает ответ.
// Непомещающееся событие никогда не обрезается: страница содержит причину и
// сохраняет курсор перед ним. Оригинал доступен по ID в явном архиве.
func ReadTeamContext(root, run string, options TeamReadOptions) (TeamContext, error) {
	chat, err := ReadTeam(root, run)
	if err != nil {
		return TeamContext{}, err
	}
	return teamContext(chat, options)
}

// teamContext не меняет chat: копии room/maps защищают сохранённые обязательства.
func teamContext(chat TeamChat, o TeamReadOptions) (TeamContext, error) {
	out := TeamContext{RunID: chat.RunID, Goal: chat.Goal, Members: chat.Members, Messages: []TeamMessage{}, Head: len(chat.Messages)}
	if o.Limit < 0 || o.Limit > 100 || o.From < 0 || o.Through < 0 || len(o.IDs) > 100 {
		return out, errors.New("неверные границы страницы (limit: 1–100)")
	}
	if o.Limit == 0 {
		o.Limit = 100
	}
	if !o.Archive && (o.From != 0 || o.Through != 0 || len(o.IDs) > 0) {
		return out, errors.New("для диапазона или ID укажите archive=true")
	}
	ids := append([]string(nil), o.IDs...)
	sort.Strings(ids)
	filter := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(o.From, o.Through, ids))))
	summary := latestTeamSummary(chat)
	boundary := 0
	if summary != nil {
		boundary = summary.Through
	}
	start, end := boundary, len(chat.Messages)
	if !o.Archive {
		out.Summary = summary
		if chat.Compaction != nil {
			out.Compaction = chat.Compaction.Pending
		}
		if chat.Room != nil {
			room := *chat.Room
			room.Catalog = nil
			room.Discussions = map[string]*TeamDiscussion{}
			for id, d := range chat.Room.Discussions {
				if d.State != "closed" {
					room.Discussions[id] = d
				}
			}
			room.Tasks = map[string]*TeamTask{}
			for id, t := range chat.Room.Tasks {
				if t.AcceptedAt == nil {
					room.Tasks[id] = t
				}
			}
			room.Actors = map[string]*TeamActor{}
			for id, a := range chat.Room.Actors {
				copy := *a
				copy.ThreadID = ""
				copy.TurnID = ""
				copy.Delivery = nil
				room.Actors[id] = &copy
			}
			out.Room = &room
		}
	} else {
		out.Goal = ""
		out.Members = nil
		start = 0
		if o.From > 0 {
			start = o.From - 1
		}
		if o.Through > 0 {
			end = o.Through
		}
		if start > end || end > len(chat.Messages) {
			return out, errors.New("архивный диапазон вне журнала")
		}
	}
	if o.Cursor != "" {
		var c contextCursor
		data, e := base64.RawURLEncoding.DecodeString(o.Cursor)
		if e == nil {
			e = json.Unmarshal(data, &c)
		}
		valid := e == nil && c.Run == chat.RunID && c.Archive == o.Archive && c.Position >= 0 && c.Position <= len(chat.Messages)
		if valid && c.Position > 0 {
			valid = chat.Messages[c.Position-1].ID == c.ID
		}
		if valid && o.Archive {
			valid = c.Filter == filter && c.Through == o.Through && c.Position >= start && c.Position <= end
		}
		if !valid || !o.Archive && c.Position < boundary {
			out = TeamContext{RunID: chat.RunID, Messages: []TeamMessage{}, Head: len(chat.Messages), ResetRequired: true}
			out.Notice = "Позиция устарела или повреждена. Повторите team_read {} для снимка; пропущенные оригиналы доступны через archive=true."
			return out, nil
		}
		start = c.Position
	}
	budget := teamContextPolicy(chat).ResponseTokens
	// В явном архиве одно большое исходное сообщение разрешено до отдельного
	// потолка. Это не расширяет лимит обычного контекста.
	if o.Archive {
		budget = 65536
	}
	base := EstimateTeamTokens(out) + 256
	if base > budget {
		return TeamContext{RunID: chat.RunID, Messages: []TeamMessage{}, Head: len(chat.Messages), Notice: "Цель и действующие поручения превышают лимит ответа; увеличьте responseTokens в contextPolicy. Переписка доступна через archive=true."}, nil
	}
	selected := map[string]bool{}
	for _, id := range o.IDs {
		found := false
		for _, m := range chat.Messages[start:end] {
			if m.ID == id {
				found = true
				break
			}
		}
		// При продолжении ID из предыдущих страниц уже позади; исходный запрос
		// проверяется по полному указанному диапазону, а не по курсору.
		if o.Cursor == "" && !found {
			return out, fmt.Errorf("источник %q не найден в диапазоне", id)
		}
		selected[id] = true
	}
	position := start
	for position < end {
		message := chat.Messages[position]
		if len(selected) > 0 && !selected[message.ID] {
			position++
			continue
		}
		message.Position = position + 1
		// Старая сводка остаётся доступна в архиве, но не дублирует текущую в обычном чтении.
		if !o.Archive {
			message.Summary = nil
			message.Goal = ""
			message.DiscussionInput = ""
		}
		cost := EstimateTeamTokens(message)
		if len(out.Messages) >= o.Limit || base+cost > budget {
			out.HasMore = true
			out.Notice = fmt.Sprintf("Есть продолжение; передайте cursor. Следующее событие: %s (позиция %d).", message.ID, position+1)
			if len(out.Messages) == 0 {
				out.Notice = "Событие превышает лимит страницы; прочитайте его по ID через archive=true, затем продолжите по resumeCursor: " + message.ID
				resume := contextCursor{Run: chat.RunID, Position: position + 1, ID: message.ID, Archive: o.Archive, Through: o.Through, Filter: filter}
				data, _ := json.Marshal(resume)
				out.ResumeCursor = base64.RawURLEncoding.EncodeToString(data)
			}
			break
		}
		out.Messages = append(out.Messages, message)
		base += cost
		position++
	}
	cursor := contextCursor{Run: chat.RunID, Position: position, Archive: o.Archive, Through: o.Through, Filter: filter}
	if position > 0 {
		cursor.ID = chat.Messages[position-1].ID
	}
	data, _ := json.Marshal(cursor)
	out.Cursor = base64.RawURLEncoding.EncodeToString(data)
	out.EstimatedTokens = EstimateTeamTokens(out) + 4
	// Проверяем готовый JSON, включая курсоры и notice. Запас при подборе
	// сообщений не является гарантией при необычно длинных ID и настройках.
	if EstimateTeamTokens(out) > budget {
		return TeamContext{}, errors.New("полный ответ превышает responseTokens; увеличьте бюджет или уменьшите limit")
	}
	return out, nil
}

// ContextReadCommand поддерживает старые threads с закреплённой schema team_read {}.
func ContextReadCommand(text string) (TeamReadOptions, bool, error) {
	if !strings.HasPrefix(text, "/context ") {
		return TeamReadOptions{}, false, nil
	}
	var options TeamReadOptions
	err := json.Unmarshal([]byte(strings.TrimPrefix(text, "/context ")), &options, json.RejectUnknownMembers(true))
	return options, true, err
}
