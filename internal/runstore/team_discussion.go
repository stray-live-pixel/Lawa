package runstore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// TeamDiscussion хранит бюджет одной причинной цепочки под team.lock.
// Считаются отдельные сообщения коллегам; отчёты Боссу бюджет не расходуют.
// Корень — входное поручение, поэтому параллельные задачи не объединяются.
type TeamDiscussion struct {
	ID         string `json:"id"`
	QuestionID string `json:"questionId"`
	Cycle      int    `json:"cycle"`
	Count      int    `json:"count"`
	State      string `json:"state"` // active, paused или closed; состояние не является приёмкой задачи.
}

const discussionLimit = 6

// discussionReply даёт явный выбор входа при смешанной доставке. Текстовый
// протокол работает и в старых Codex thread с неизменяемой схемой team_post.
func discussionReply(text string) (string, string, error) {
	if !strings.HasPrefix(text, "/reply ") {
		return "", text, nil
	}
	parts := strings.SplitN(strings.TrimPrefix(text, "/reply "), " ", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", "", errors.New("формат: /reply ID_входного_сообщения @id текст")
	}
	return parts[0], strings.TrimSpace(parts[1]), nil
}

// discussionSource восстанавливает корень старой переписки без изменения истории.
// Неоднозначные старые связи требуют решения Босса, а не слияния бюджетов.
func discussionSource(chat *TeamChat, id string, seen map[string]bool) (string, int, error) {
	if seen[id] {
		return "", 0, errors.New("цикл ссылок сообщений")
	}
	seen[id] = true
	defer delete(seen, id)
	for _, m := range chat.Messages {
		if m.ID != id {
			continue
		}
		if m.DiscussionID != "" {
			return m.DiscussionID, m.DiscussionCycle, nil
		}
		if m.AuthorID == "boss" || m.AuthorID == "human" || len(m.ReplyToIDs) == 0 {
			return m.ID, 1, nil
		}
		root, cycle := "", 0
		for _, parent := range m.ReplyToIDs {
			next, n, err := discussionSource(chat, parent, seen)
			if err != nil {
				return "", 0, err
			}
			if root != "" && (root != next || cycle != n) {
				return "", 0, errors.New("старая переписка смешивает цепочки; передай вопрос Боссу")
			}
			root, cycle = next, n
		}
		return root, cycle, nil
	}
	return "", 0, errors.New("не найден источник обсуждения")
}

// attachDiscussion выбирает ровно один входящий контекст до записи сообщения.
// Нельзя сбросить бюджет сменой адресата или продолжить старый ход после решения Босса.
func attachDiscussion(chat *TeamChat, m *TeamMessage, source string) error {
	if m.AuthorID == "human" || m.AuthorID == "boss" {
		if source != "" {
			return errors.New("/reply предназначен для сотрудников")
		}
		return nil
	}
	actor := chat.Room.Actors[m.AuthorID]
	if source != "" && !slices.Contains(actor.Delivery.IDs, source) {
		return errors.New("/reply требует ID из текущей доставки")
	}
	if m.To == "boss" {
		if source != "" {
			m.ReplyTo, m.ReplyToIDs = source, []string{source}
		}
		return nil
	}
	inputs := actor.Delivery.IDs
	if source != "" {
		inputs = []string{source}
	}
	root, cycle := "", 0
	for _, input := range inputs {
		next, n, err := discussionSource(chat, input, map[string]bool{})
		if err != nil {
			return err
		}
		if root != "" && (root != next || cycle != n) {
			return errors.New("несколько цепочек: используй /reply ID_входного_сообщения @id текст")
		}
		root, cycle = next, n
	}
	if root == "" {
		return errors.New("нет источника обсуждения")
	}
	if chat.Room.Discussions == nil {
		chat.Room.Discussions = map[string]*TeamDiscussion{}
	}
	d := chat.Room.Discussions[root]
	if d == nil {
		d = &TeamDiscussion{ID: root, QuestionID: m.ID, Cycle: 1, State: "active"}
		chat.Room.Discussions[root] = d
	}
	if d.State != "active" || cycle != d.Cycle {
		return errors.New("цепочка остановлена или этот ход относится к прошлому циклу; передай результат @boss")
	}
	m.DiscussionID, m.DiscussionCycle = root, cycle
	if source != "" {
		m.ReplyTo, m.ReplyToIDs = source, []string{source}
	}
	d.Count++
	return nil
}

// pauseDiscussion фиксирует шестую реплику и одну эскалацию в той же транзакции.
// Уже запущенные ходы не отменяются, но новые сообщения коллегам будут отклонены.
// Suppressed остаётся навсегда: возобновление создаёт новый вход, не проигрывает старые.
func pauseDiscussion(chat *TeamChat, m *TeamMessage) {
	d := chat.Room.Discussions[m.DiscussionID]
	if d == nil || d.Count < discussionLimit || d.State != "active" {
		return
	}
	d.State = "paused"
	ids := []string{d.QuestionID}
	var positions []string
	for i := range chat.Messages {
		msg := &chat.Messages[i]
		if msg.DiscussionID != d.ID || msg.DiscussionCycle != d.Cycle {
			continue
		}
		if !slices.Contains(ids, msg.ID) {
			ids = append(ids, msg.ID)
		}
		positions = append(positions, fmt.Sprintf("%s → %s [%s]: %s", msg.AuthorID, msg.To, msg.ID, msg.Text))
		msg.Suppressed = true
	}
	m.Suppressed = true
	question := ""
	for _, msg := range chat.Messages {
		if msg.ID == d.QuestionID {
			question = msg.Text
		}
	}
	event := TeamMessage{
		ID:       fmt.Sprintf("discussion-%x-%d", sha256.Sum256([]byte(d.ID)), d.Cycle),
		AuthorID: "system", To: "boss", Kind: "discussion_escalation", Date: m.Date,
		ReplyTo: d.QuestionID, ReplyToIDs: ids,
		Text: fmt.Sprintf("@boss Цепочка %s, цикл %d: достигнут лимит 6 отдельных сообщений коллегам. Автоматическое продолжение остановлено. Исходный вопрос [%s]: %s\nПозиции и ссылки:\n%s\nРеши через team_post: /discussion %s %d close <решение> или /discussion %s %d resume @id <указания>.", d.ID, d.Cycle, d.QuestionID, question, strings.Join(positions, "\n"), d.ID, d.Cycle, d.ID, d.Cycle),
	}
	chat.Members["system"] = TeamMember{Name: "Lawa"}
	chat.Messages = append(chat.Messages, event)
	wakeForMessage(chat, event)
}

// decideDiscussion — явное решение работающего Босса. Номер цикла защищает
// от запоздалого решения; ID вызова делает повтор безопасным даже после рестарта.
// Закрытие вопроса не принимает обязательства, возобновление не создаёт новых.
func decideDiscussion(chat *TeamChat, author, id, input string) (TeamMessage, error) {
	for _, m := range chat.Messages {
		if m.ID == id {
			if m.AuthorID != author || m.DiscussionInput != input {
				return TeamMessage{}, errors.New("ID занят другим сообщением")
			}
			return m, nil
		}
	}
	boss := chat.Room.Actors["boss"]
	if author != "boss" || boss == nil || boss.Status != "working" || boss.Delivery == nil || chat.Room.AchievedAt != nil {
		return TeamMessage{}, errors.New("решение обсуждения доступно только работающему Боссу")
	}
	parts := strings.SplitN(input, " ", 5)
	if len(parts) != 5 || id == "" || len(id) > 200 || len(input) > 8192 || !utf8.ValidString(input) || !utf8.ValidString(id) {
		return TeamMessage{}, errors.New("формат: /discussion ID цикл close решение | resume @id указания")
	}
	cycle, err := strconv.Atoi(parts[2])
	d := chat.Room.Discussions[parts[1]]
	if err != nil || d == nil || d.Cycle != cycle || d.State != "paused" {
		return TeamMessage{}, errors.New("нет приостановленного обсуждения с этим ID и циклом")
	}
	text := strings.TrimSpace(parts[4])
	if len(strings.Fields(text)) == 0 || len(strings.Fields(text)) > 50 {
		return TeamMessage{}, errors.New("решение: 1–50 слов")
	}
	m := TeamMessage{ID: id, AuthorID: author, Kind: "discussion_decision", Text: text, DiscussionInput: input, DiscussionID: d.ID, DiscussionCycle: cycle, ReplyTo: d.QuestionID, ReplyToIDs: []string{d.QuestionID}, Date: time.Now().UTC()}
	switch parts[3] {
	case "close":
		d.State = "closed"
	case "resume":
		to, err := addressedTo(text)
		if err != nil || to == "" || to == "boss" || to == "human" || chat.Room.Actors[to] == nil {
			return TeamMessage{}, errors.New("укажи приглашённого сотрудника: resume @id указания")
		}
		d.State, d.Count, d.Cycle = "active", 0, d.Cycle+1
		m.To, m.DiscussionCycle = to, d.Cycle
	default:
		return TeamMessage{}, errors.New("решение должно быть close или resume")
	}
	chat.Messages = append(chat.Messages, m)
	wakeForMessage(chat, m)
	return m, nil
}

// initializeDiscussions переносит старые цепочки при первой записи/доставке.
// Переписка до последнего завершения цели не возобновляется. Неоднозначные
// связи остаются без изменений: discussionSource потребует обращения к Боссу.
func initializeDiscussions(chat *TeamChat) {
	if chat.Room.Discussions != nil || chat.Room.AchievedAt != nil {
		return
	}
	chat.Room.Discussions = map[string]*TeamDiscussion{}
	start := 0
	for i, m := range chat.Messages {
		if m.Kind == "achievement" {
			start = i + 1
		}
	}
	for i := start; i < len(chat.Messages); i++ {
		m := &chat.Messages[i]
		if m.AuthorID == "boss" || m.AuthorID == "human" || m.AuthorID == "system" || m.To == "" || m.To == "boss" || m.To == "human" {
			continue
		}
		root, cycle, err := discussionSource(chat, m.ID, map[string]bool{})
		if err != nil {
			continue
		}
		m.DiscussionID, m.DiscussionCycle = root, cycle
		d := chat.Room.Discussions[root]
		if d == nil {
			d = &TeamDiscussion{ID: root, QuestionID: m.ID, Cycle: 1, State: "active"}
			chat.Room.Discussions[root] = d
		}
		d.Count = min(d.Count+1, discussionLimit)
	}
	// Эскалации дописываются после обхода: указатели на элементы среза не переживают append.
	for _, d := range chat.Room.Discussions {
		if d.Count == discussionLimit {
			pauseDiscussion(chat, &TeamMessage{DiscussionID: d.ID, Date: time.Now().UTC()})
		}
	}
}

// TeamDeliveryDiscussionStopped отличает молчание сотрудника от остановки всех
// его входов уже переданной Боссу цепочкой. Смешанный ход с независимой задачей
// по-прежнему обязан дать адресный ответ; технические ошибки не скрываются.
func TeamDeliveryDiscussionStopped(chat *TeamChat, actorID string) bool {
	actor := chat.Room.Actors[actorID]
	if actor == nil || actor.Delivery == nil || len(actor.Delivery.IDs) == 0 {
		return false
	}
	for _, id := range actor.Delivery.IDs {
		root, cycle, err := discussionSource(chat, id, map[string]bool{})
		d := chat.Room.Discussions[root]
		if err != nil || d == nil || d.State == "active" && d.Cycle == cycle {
			return false
		}
	}
	return true
}
