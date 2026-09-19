package runstore

import "slices"

// TeamMessageForActor учитывает копию обращения Боссу при возобновлении через
// сотрудника и исключает остановленные обсуждения во всех путях доставки.
// Текст не переписывается и второго сообщения от Чела не возникает.
func TeamMessageForActor(m TeamMessage, id string) bool {
	return !m.Suppressed && (m.To == id || id == "boss" && m.AuthorID == "human" && m.NotifyBoss)
}

// TeamHasUrgentMessages выделяет обращения Чела и связанные ответы, которые
// нельзя поглотить завершением цели. Для пробуждения подходят все адресные сообщения.
func TeamHasUrgentMessages(chat TeamChat, id string, from int) bool {
	for _, m := range chat.Messages[from:] {
		if !TeamMessageForActor(m, id) {
			continue
		}
		if m.AuthorID == "human" || m.AuthorID == "system" && m.To == "boss" {
			return true
		}
		if id == "boss" && chat.Room.Actors[m.AuthorID] != nil && m.AuthorID != "boss" {
			for _, source := range chat.Messages {
				if (source.ID == m.ReplyTo || slices.Contains(m.ReplyToIDs, source.ID)) && source.AuthorID == "human" {
					return true
				}
			}
		}
	}
	return false
}

// TeamHasPendingMessages проверяет адресный хвост после текущей порции.
// Вызывается под team.lock: сообщения до End уже доставлены, новые нельзя
// потерять при завершении или восстановлении хода.
func TeamHasPendingMessages(chat TeamChat, id string, from int) bool {
	for _, m := range chat.Messages[from:] {
		if TeamMessageForActor(m, id) {
			return true
		}
	}
	return false
}

// wakeForMessage изменяет только расписание, не сбрасывает Delivery/ошибки.
// При занятом сотруднике finish проверит непрочитанный хвост; неоднозначный
// старый turn остаётся заблокированным, даже если Чел прислал новое поручение.
func wakeForMessage(chat *TeamChat, m TeamMessage) {
	if chat.Room.AchievedAt != nil {
		return
	}
	for id, a := range chat.Room.Actors {
		if !TeamMessageForActor(m, id) || a.Delivery != nil || a.Status == "blocked" {
			continue
		}
		a.NextCheck = m.Date
	}
}

// reopenForHuman вызывается только после валидации нового сообщения и до его
// вставки. Поэтому retry не будит сотрудников повторно. Без тега цель достигнута.
func reopenForHuman(chat *TeamChat, m *TeamMessage) {
	if m.AuthorID != "human" || chat.Room.Actors[m.To] == nil {
		return
	}
	// Прямой вопрос любому сотруднику всегда проходит через Босса,
	// в том числе до первого завершения цели.
	m.NotifyBoss = m.To != "boss"
	if chat.Room.AchievedAt != nil {
		chat.Room.AchievedAt = nil
		chat.Members["system"] = TeamMember{Name: "Lawa"}
		chat.Messages = append(chat.Messages, TeamMessage{ID: "reopen-" + m.ID, AuthorID: "system", Kind: "system", Date: m.Date, Text: "Чел возобновил работу команды."})
	}
}

// Проверка нулевого времени отдельно от Before: завершившийся сотрудник спит
// до нового обращения, а не проверяет чат непрерывно от нулевой даты.
func TeamActorSleeping(a *TeamActor) bool { return a.Delivery == nil && a.NextCheck.IsZero() }
