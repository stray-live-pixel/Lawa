package runstore

import "slices"

// TeamMessageForActor учитывает копию обращения Боссу при возобновлении через
// Разработчика. Текст не переписывается и второго сообщения от Чела не возникает.
func TeamMessageForActor(m TeamMessage, id string) bool {
	return m.To == id || id == "boss" && m.AuthorID == "human" && m.NotifyBoss
}

// TeamHasUrgentMessages сохраняет немедленное пробуждение, если обращение Чела
// пришло во время turn. Ответ сотрудника на такое обращение будит Босса тоже.
func TeamHasUrgentMessages(chat TeamChat, id string, from int) bool {
	for _, m := range chat.Messages[from:] {
		if !TeamMessageForActor(m, id) {
			continue
		}
		if m.AuthorID == "human" {
			return true
		}
		if id == "boss" && m.AuthorID == "developer" {
			for _, source := range chat.Messages {
				if (source.ID == m.ReplyTo || slices.Contains(m.ReplyToIDs, source.ID)) && source.AuthorID == "human" {
					return true
				}
			}
		}
	}
	return false
}

// pendingHumanRelay не позволяет Боссу выдать ответ за сотрудника до его
// фактической реплики. Ответ проверяется по replyTo, а не по словам «готово».
func pendingHumanRelay(chat TeamChat) bool {
	for _, m := range chat.Messages {
		if m.AuthorID != "human" || !m.NotifyBoss {
			continue
		}
		answered := false
		for _, reply := range chat.Messages {
			if reply.AuthorID == "developer" && reply.To == "boss" && (reply.ReplyTo == m.ID || slices.Contains(reply.ReplyToIDs, m.ID)) {
				answered = true
				break
			}
		}
		if !answered {
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
		if m.AuthorID == "human" || a.NextCheck.IsZero() || TeamHasUrgentMessages(*chat, id, len(chat.Messages)-1) {
			a.NextCheck = m.Date
		}
	}
}

// reopenForHuman вызывается только после валидации нового сообщения и до его
// вставки. Поэтому retry не будит сотрудников повторно. Без тега цель достигнута.
func reopenForHuman(chat *TeamChat, m *TeamMessage) {
	if m.AuthorID != "human" || (m.To != "boss" && m.To != "developer") {
		return
	}
	if chat.Room.AchievedAt != nil {
		m.NotifyBoss = m.To == "developer"
		chat.Room.AchievedAt = nil
		chat.Members["system"] = TeamMember{Name: "Lawa"}
		chat.Messages = append(chat.Messages, TeamMessage{ID: "reopen-" + m.ID, AuthorID: "system", Kind: "system", Date: m.Date, Text: "Чел возобновил работу команды."})
	}
}

// Проверка нулевого времени отдельно от Before: завершившийся сотрудник спит
// до нового обращения, а не проверяет чат непрерывно от нулевой даты.
func TeamActorSleeping(a *TeamActor) bool { return a.Delivery == nil && a.NextCheck.IsZero() }
