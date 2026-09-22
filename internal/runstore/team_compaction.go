package runstore

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// TeamContextPolicy хранит настраиваемые инженерные бюджеты в team.json.
// Нулевые значения используют defaults; порог всегда больше свежего участка.
type TeamContextPolicy struct {
	ThresholdTokens int `json:"thresholdTokens"`
	RecentTokens    int `json:"recentTokens"`
	SummaryTokens   int `json:"summaryTokens"`
	ResponseTokens  int `json:"responseTokens"`
}

// teamContextPolicy дополняет частичную конфигурацию и исключает пустой цикл сжатия.
func teamContextPolicy(chat TeamChat) TeamContextPolicy {
	p := TeamContextPolicy{12000, 4000, 2000, 12000}
	if c := chat.ContextPolicy; c != nil {
		if c.ThresholdTokens > 0 {
			p.ThresholdTokens = c.ThresholdTokens
		}
		if c.RecentTokens > 0 {
			p.RecentTokens = c.RecentTokens
		}
		if c.SummaryTokens > 0 {
			p.SummaryTokens = c.SummaryTokens
		}
		if c.ResponseTokens > 0 {
			p.ResponseTokens = c.ResponseTokens
		}
	}
	if p.ThresholdTokens <= p.RecentTokens {
		p.ThresholdTokens = p.RecentTokens * 3
	}
	return p
}

// TeamSummary — неизменяемая версия прошлого до Through включительно.
// RequestID вместе с предыдущей версией защищает от повтора и устаревшей записи.
type TeamSummary struct {
	RequestID string   `json:"requestId"`
	Through   int      `json:"through"`
	Text      string   `json:"text"`
	SourceIDs []string `json:"sourceIds"`
}

// TeamCompactionRequest фиксирует диапазон до запуска модели. Новые сообщения
// после Through никогда не входят в эту попытку и не скрываются её публикацией.
type TeamCompactionRequest struct {
	ID         string `json:"id"`
	PreviousID string `json:"previousId,omitempty"`
	From       int    `json:"from"`
	Through    int    `json:"through"`
	Attempt    int    `json:"attempt"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// TeamCompaction хранит только текущую работу; все успешные версии — в событиях.
type TeamCompaction struct {
	Pending *TeamCompactionRequest `json:"pending,omitempty"`
}

// ensureTeamCompaction вызывается под team.lock после изменения и scheduler-ом
// для старых заказов. Сеть здесь запрещена. Один адресный запрос будит Босса;
// failed блокирует автоматические повторы до явного восстановления оператором.
func ensureTeamCompaction(chat *TeamChat, now time.Time) {
	if chat.Room == nil || chat.Room.AchievedAt != nil || chat.Room.Actors["boss"] == nil {
		return
	}
	if chat.Compaction != nil && chat.Compaction.Pending != nil {
		return
	}
	previous := latestTeamSummary(*chat)
	start := 0
	previousID := ""
	if previous != nil {
		start = previous.Through
		previousID = previous.RequestID
	}
	p := teamContextPolicy(*chat)
	total := 0
	for _, m := range chat.Messages[start:] {
		total += EstimateTeamTokens(m)
	}
	if total < p.ThresholdTokens {
		return
	}
	through := len(chat.Messages)
	recent := 0
	for through > start && recent < p.RecentTokens {
		through--
		recent += EstimateTeamTokens(chat.Messages[through])
	}
	if through <= start {
		return
	}
	request := &TeamCompactionRequest{ID: fmt.Sprintf("compact-%d-%d", through, len(chat.Messages)+1), PreviousID: previousID, From: start + 1, Through: through, Attempt: 1, Status: "pending"}
	chat.Compaction = &TeamCompaction{Pending: request}
	enqueueCompaction(chat, request, now)
}

// enqueueCompaction использует обычное пробуждение; занятый Босс получит запрос
// после текущего turn, остальная команда продолжает работать без общей паузы.
func enqueueCompaction(chat *TeamChat, r *TeamCompactionRequest, now time.Time) {
	m := TeamMessage{ID: r.ID, AuthorID: "system", To: "boss", Kind: "compaction_request", Date: now.UTC(), Text: fmt.Sprintf("@boss Сожми прошлую переписку: запрос %s, позиции %d–%d. Прочитай предыдущую сводку и весь диапазон через /context, затем опубликуй /compact. До публикации оригиналы остаются в обычном чтении.", r.ID, r.From, r.Through)}
	chat.Members["system"] = TeamMember{Name: "Lawa"}
	chat.Messages = append(chat.Messages, m)
	wakeForMessage(chat, m)
}

// EnsureTeamCompaction нужен для переписки, накопленной до обновления приложения.
// Проверка без записи на каждом тике не раздувает историю или IO.
func EnsureTeamCompaction(ctx context.Context, root, run string) error {
	chat, err := ReadTeam(root, run)
	if err != nil {
		return err
	}
	before := len(chat.Messages)
	ensureTeamCompaction(&chat, time.Now())
	if len(chat.Messages) == before {
		return nil
	}
	return UpdateTeam(ctx, root, run, func(*TeamChat) error { return nil })
}

// FinishTeamCompaction вызывается до очистки Delivery. Единственная повторная
// попытка — только после доказанного завершения хода без публикации. Неоднозначный
// сетевой сбой остаётся в существующем механизме blocked/recover runtime.
func FinishTeamCompaction(chat *TeamChat, id string, now time.Time) {
	if id != "boss" || chat.Compaction == nil || chat.Compaction.Pending == nil {
		return
	}
	r := chat.Compaction.Pending
	boss := chat.Room.Actors["boss"]
	if boss.Delivery == nil || !slices.Contains(boss.Delivery.IDs, r.ID) || r.Status != "pending" {
		return
	}
	if r.Attempt >= 2 {
		r.Status = "failed"
		r.Error = "Босс дважды завершил ход без сводки. Нужна явная команда /compact_retry."
		return
	}
	r.Attempt++
	r.ID += "-retry"
	enqueueCompaction(chat, r, now)
}

// PublishTeamSummary атомарно добавляет версию и снимает запрос. Повтор с тем же
// содержимым возвращает прежнее событие, отличающееся или устаревшее отклоняется.
// Семантическую полноту проверяет Босс; сервер проверяет автора, границы и источники.
func PublishTeamSummary(ctx context.Context, root, run, author string, input TeamSummary) (TeamMessage, error) {
	var result TeamMessage
	err := UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if author != "boss" || chat.Room == nil {
			return errors.New("сводку публикует только Босс комнаты")
		}
		for _, m := range chat.Messages {
			if m.Summary != nil && m.Summary.RequestID == input.RequestID {
				if m.Summary.Through != input.Through || m.Summary.Text != input.Text || !slices.Equal(m.Summary.SourceIDs, input.SourceIDs) {
					return errors.New("повтор отличается от опубликованной сводки")
				}
				result = m
				return nil
			}
		}
		boss := chat.Room.Actors["boss"]
		if boss == nil || boss.Status != "working" || boss.Delivery == nil {
			return errors.New("нужен активный ход Босса")
		}
		if chat.Compaction == nil || chat.Compaction.Pending == nil {
			return errors.New("нет запроса компактификации")
		}
		r := chat.Compaction.Pending
		previousID := ""
		if previous := latestTeamSummary(*chat); previous != nil {
			previousID = previous.RequestID
		}
		if r.Status != "pending" || r.ID != input.RequestID || r.Through != input.Through || r.PreviousID != previousID {
			return errors.New("устаревшая попытка компактификации; прочитайте текущий запрос")
		}
		if strings.TrimSpace(input.Text) == "" || !utf8.ValidString(input.Text) || len(input.SourceIDs) == 0 || EstimateTeamTokens(input) > teamContextPolicy(*chat).SummaryTokens {
			return errors.New("нужны непустая сводка, ID источников и соблюдение summaryTokens")
		}
		sources := map[string]bool{}
		for _, m := range chat.Messages[:r.Through] {
			sources[m.ID] = true
		}
		for _, id := range input.SourceIDs {
			if !sources[id] {
				return fmt.Errorf("источник %q вне сворачиваемого прошлого", id)
			}
		}
		result = TeamMessage{ID: "summary-" + r.ID, AuthorID: "boss", Kind: "compaction", Date: time.Now().UTC(), Text: fmt.Sprintf("Босс сжал прошлую переписку до позиции %d. Оригиналы сохранены в архиве.", r.Through), Summary: &input}
		chat.Messages = append(chat.Messages, result)
		chat.Compaction.Pending = nil
		return nil
	})
	return result, err
}

// RetryTeamCompaction — явное восстановление после двух законченных попыток.
// Не снимает blocked неоднозначного turn: его восстановление выполняется отдельно.
func RetryTeamCompaction(ctx context.Context, root, run, author, id string) (TeamMessage, error) {
	var result TeamMessage
	err := UpdateTeam(ctx, root, run, func(chat *TeamChat) error {
		if chat.Room == nil || (author != "human" && author != "boss") {
			return errors.New("повтор разрешён Боссу или Челу")
		}
		if id == "" || len(id) > 200 || !utf8.ValidString(id) {
			return errors.New("нужен ID повторной команды до 200 байт")
		}
		requestID := "compact-manual-" + id
		for _, m := range chat.Messages {
			if m.ID == requestID {
				if m.Kind != "compaction_request" || m.AuthorID != "system" {
					return errors.New("ID повтора уже занят")
				}
				result = m
				return nil
			}
		}
		if chat.Compaction == nil || chat.Compaction.Pending == nil || chat.Compaction.Pending.Status != "failed" {
			return errors.New("нет завершившейся ошибкой компактификации")
		}
		if chat.Room.AchievedAt != nil {
			return errors.New("сначала возобновите цель обращением к Боссу")
		}
		r := chat.Compaction.Pending
		r.Status = "pending"
		r.Error = ""
		r.Attempt = 1
		r.ID = requestID
		enqueueCompaction(chat, r, time.Now())
		result = chat.Messages[len(chat.Messages)-1]
		return nil
	})
	return result, err
}

// ParseTeamSummary — транспорт старых threads: team_post schema уже принимает
// произвольную строку. Сводка не подчиняется ограничению обычной реплики 50 слов.
func ParseTeamSummary(text string) (TeamSummary, error) {
	var in TeamSummary
	err := json.Unmarshal([]byte(strings.TrimPrefix(text, "/compact ")), &in, json.RejectUnknownMembers(true))
	return in, err
}

// validateTeamContext не позволяет повреждённой границе скрыть исходники или
// вызвать выход за срез. Старые файлы без сводок проходят без миграции.
func validateTeamContext(chat TeamChat) error {
	through := 0
	requests := map[string]bool{}
	for i, m := range chat.Messages {
		if s := m.Summary; s != nil {
			if m.Kind != "compaction" || m.AuthorID != "boss" || s.RequestID == "" || requests[s.RequestID] || s.Through <= through || s.Through > i || strings.TrimSpace(s.Text) == "" {
				return errors.New("повреждена версия сводки команды")
			}
			requests[s.RequestID] = true
			through = s.Through
		}
	}
	if chat.Compaction != nil && chat.Compaction.Pending != nil {
		r := chat.Compaction.Pending
		previousID := ""
		if s := latestTeamSummary(chat); s != nil {
			previousID = s.RequestID
		}
		if r.ID == "" || r.PreviousID != previousID || r.From != through+1 || r.Through < r.From || r.Through > len(chat.Messages) || (r.Status != "pending" && r.Status != "failed") || r.Attempt < 1 || r.Attempt > 2 {
			return errors.New("повреждён запрос компактификации")
		}
	}
	return nil
}
