package runstore

import (
	"testing"
	"time"
)

// Изменяемые акторы и служебные курсоры не должны переписывать прошлое.
// При откате часов порядок кадров остаётся монотонным.
func TestTeamFramesAreImmutableAndDeduplicated(t *testing.T) {
	now := time.Now().UTC()
	chat := TeamChat{Room: &TeamRoom{Actors: map[string]*TeamActor{"boss": {Status: "idle"}}}}
	recordTeamFrame(&chat, now)
	chat.Room.Actors["boss"].ThreadID = "thread"
	recordTeamFrame(&chat, now.Add(time.Second))
	if len(chat.History.Frames) != 1 {
		t.Fatal("служебная правка создала кадр")
	}
	chat.Room.Actors["boss"].Status = "working"
	chat.Room.Actors["boss"].Summary = "Проверяет игру"
	chat.Messages = append(chat.Messages, TeamMessage{ID: "task"})
	recordTeamFrame(&chat, now.Add(-time.Second))
	frames := chat.History.Frames
	if len(frames) != 2 || frames[0].Actors["boss"].Status != "idle" || frames[1].MessageCount != 1 || !frames[1].At.Equal(now) {
		t.Fatalf("кадры: %+v", frames)
	}
}
