package runstore

import (
	"testing"
	"time"
)

// Причина меняется вместе с фактами; чтение и повторная транзакция не начинают
// длительность заново. Новые сообщения имеют приоритет над смысловым ожиданием.
func TestWaitTransitions(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	a := &TeamActor{Status: "idle"}
	chat := TeamChat{Room: &TeamRoom{Actors: map[string]*TeamActor{"developer": a}}}
	RefreshTeamWaits(&chat, now)
	if a.Wait.Kind != "no_messages" {
		t.Fatal(a.Wait)
	}
	RefreshTeamWaits(&chat, now.Add(time.Minute))
	if !a.Wait.Since.Equal(now) {
		t.Fatal("таймер сброшен")
	}
	chat.Messages = append(chat.Messages, TeamMessage{ID: "task", To: "developer"})
	RefreshTeamWaits(&chat, now.Add(2*time.Minute))
	if a.Wait.Kind != "queue" {
		t.Fatal(a.Wait)
	}
	a.CapacityPending = true
	RefreshTeamWaits(&chat, now.Add(3*time.Minute))
	if a.Wait.Kind != "capacity" {
		t.Fatal(a.Wait)
	}
	a.Delivery = &TeamDelivery{End: 1}
	a.Status = "working"
	RefreshTeamWaits(&chat, now)
	if a.Wait != nil {
		t.Fatal("запущенный ход продолжает ждать слот")
	}
	a.ApprovalPending = true
	RefreshTeamWaits(&chat, now)
	if a.Wait.Kind != "permission" {
		t.Fatal(a.Wait)
	}
	a.ApprovalPending = false
	a.Status = "blocked"
	a.Error = "сбой"
	RefreshTeamWaits(&chat, now)
	if a.Wait.Kind != "error" || a.Wait.Text != "сбой" {
		t.Fatal(a.Wait)
	}
	chat.Room.AchievedAt = &now
	RefreshTeamWaits(&chat, now)
	if a.Wait != nil {
		t.Fatal("завершённая цель ждёт")
	}
}

// Приёмка связывается с точным отчётом; ответ снимает только зависимость от
// указанного коллеги. Кадр хранит копию причины и не меняется задним числом.
func TestDependencyResolutionAndHistory(t *testing.T) {
	now := time.Now()
	a := &TeamActor{Status: "idle", Cursor: 1, Dependency: &TeamWait{Kind: "result", Source: "actor", ActorID: "reviewer", MessageID: "question", Since: now}}
	chat := TeamChat{Messages: []TeamMessage{{ID: "question", AuthorID: "developer", To: "reviewer"}}, Room: &TeamRoom{Actors: map[string]*TeamActor{"developer": a}}}
	RefreshTeamWaits(&chat, now)
	frame := CaptureTeamFrame(chat, now)
	if a.Wait.Kind != "result" {
		t.Fatal(a.Wait)
	}
	a.Wait.Text = "позже"
	if frame.Actors["developer"].Wait.Text != "" {
		t.Fatal("кадр изменился")
	}
	chat.Messages = append(chat.Messages, TeamMessage{ID: "answer", AuthorID: "reviewer", To: "developer"})
	RefreshTeamWaits(&chat, now)
	if a.Dependency != nil || a.Wait.Kind != "queue" {
		t.Fatal(a)
	}
	a.Cursor = len(chat.Messages)
	a.Dependency = &TeamWait{Kind: "acceptance", Source: "actor", ActorID: "boss", MessageID: "report", Since: now}
	chat.Messages = append(chat.Messages, TeamMessage{ID: "report", AuthorID: "developer", To: "boss"})
	RefreshTeamWaits(&chat, now)
	if a.Wait.Kind != "acceptance" {
		t.Fatal(a.Wait)
	}
	chat.Messages = append(chat.Messages, TeamMessage{Kind: "task_accepted", ResultID: "another"})
	RefreshTeamWaits(&chat, now)
	if a.Dependency == nil {
		t.Fatal("чужая приёмка сняла ожидание")
	}
	chat.Messages = append(chat.Messages, TeamMessage{Kind: "task_accepted", ResultID: "report"})
	RefreshTeamWaits(&chat, now)
	if a.Dependency != nil || a.Wait.Kind != "no_messages" {
		t.Fatal(a)
	}
}
