package runstore

import (
	"time"
)

// TeamHistory хранит только наблюдаемое состояние офиса. Frames публикуются
// атомарно вместе с чатом, поэтому статус и число сообщений никогда не расходятся.
// До RecordedFrom возможна восстановленная история с точностью времён Codex.
type TeamHistory struct {
	RecordedFrom time.Time   `json:"recordedFrom"`
	Recovered    bool        `json:"recovered"`
	Frames       []TeamFrame `json:"frames"`
}

// TeamFrame — неизменяемый кадр. MessageCount ссылается на префикс append-only
// переписки. В кадре нет thread/turn ID, доставки и внутренних рассуждений.
type TeamFrame struct {
	Goal         string                   `json:"goal,omitempty"`
	AchievedAt   *time.Time               `json:"achievedAt,omitempty"`
	At           time.Time                `json:"at"`
	MessageCount int                      `json:"messageCount"`
	Actors       map[string]TeamActorView `json:"actors"`
}

// TeamActorView содержит только публичное состояние сотрудника в кадре.
type TeamActorView struct {
	Wait      *TeamWait `json:"wait,omitempty"`
	Status    string    `json:"status"`
	Summary   string    `json:"summary,omitempty"`
	Error     string    `json:"error,omitempty"`
	NextCheck time.Time `json:"nextCheck,omitempty"`
}

// CaptureTeamFrame копирует публичные значения, не сохраняя изменяемые указатели
// на акторов runtime. Поздняя правка личности не меняет исторический кадр.
func CaptureTeamFrame(chat TeamChat, at time.Time) TeamFrame {
	frame := TeamFrame{Goal: chat.Goal, At: at.UTC(), MessageCount: len(chat.Messages), Actors: map[string]TeamActorView{}}
	if chat.Room != nil {
		if chat.Room.AchievedAt != nil {
			at := *chat.Room.AchievedAt
			frame.AchievedAt = &at
		}
		for id, a := range chat.Room.Actors {
			if a == nil {
				continue
			}
			var wait *TeamWait
			if a.Wait != nil {
				copy := *a.Wait
				wait = &copy
			}
			frame.Actors[id] = TeamActorView{Wait: wait, Status: a.Status, Summary: a.Summary, Error: a.Error, NextCheck: a.NextCheck}
		}
	}
	return frame
}

// recordTeamFrame не плодит кадры для thread ID или повторов tool-call. Часы
// могут отступить после синхронизации ОС: порядок журнала важнее wall clock.
func recordTeamFrame(chat *TeamChat, at time.Time) {
	if chat.Room == nil {
		return
	}
	frame := CaptureTeamFrame(*chat, at)
	if chat.History == nil {
		chat.History = &TeamHistory{RecordedFrom: frame.At}
	}
	frames := chat.History.Frames
	if len(frames) > 0 {
		last := frames[len(frames)-1]
		if frame.Goal == last.Goal && frame.MessageCount == last.MessageCount && sameAchievement(frame.AchievedAt, last.AchievedAt) && sameVisibleActors(frame.Actors, last.Actors) {
			return
		}
		if frame.At.Before(last.At) {
			frame.At = last.At
		}
	}
	chat.History.Frames = append(frames, frame)
}

// Личный таймер меняется даже при пустом опросе. Такие тики не являются
// событиями плеера; NextCheck сохраняется лишь вместе с настоящим изменением.
func sameVisibleActors(a, b map[string]TeamActorView) bool {
	if len(a) != len(b) {
		return false
	}
	for id, actor := range a {
		previous, ok := b[id]
		if !ok || actor.Status != previous.Status || actor.Summary != previous.Summary || actor.Error != previous.Error || !sameWait(actor.Wait, previous.Wait) {
			return false
		}
	}
	return true
}

// Отметка цели — часть кадра, поэтому при перемотке в прошлое галочка исчезает.
func sameAchievement(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

// sameWait включает начало причины, чтобы повторное ожидание стало новым кадром.
func sameWait(a, b *TeamWait) bool {
	return a == nil && b == nil || sameWaitReason(a, b) && a.Since.Equal(b.Since)
}
