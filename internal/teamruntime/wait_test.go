package teamruntime

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// Старые thread объявляют ожидание через уже известный team_post. Ссылка
// проверяется сервером; чтение не пишет файл и не делает дополнительный turn.
func TestDeclaredWaitReadAndNewDelivery(t *testing.T) {
	e, run, client, now := messageRoom(t)
	postAssignment(t, e, run, "task")
	client.execute = func(_ context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "dev", "one")
		report, err := runstore.PostActor(t.Context(), e.Root, run, "developer", "report", "@boss Готово, проверь результат")
		if err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]string{"text": `/wait {"kind":"acceptance","actor_id":"boss","message_id":"` + report.ID + `","text":"Проверь результат"}`})
		if _, err := c.CallDynamicTool(t.Context(), codex.DynamicToolCall{Tool: "team_post", Arguments: args}); err != nil {
			t.Fatal(err)
		}
		if err := runstore.SetTeamWait(t.Context(), e.Root, run, "developer", "result", "boss", "task", "чужое сообщение"); err == nil {
			t.Fatal("принята чужая ссылка")
		}
		return codex.Result{Status: "completed"}, nil
	}
	if err := e.Process(t.Context(), run, "developer"); err != nil {
		t.Fatal(err)
	}
	a := readChat(t, e, run).Room.Actors["developer"]
	if a.Wait == nil || a.Wait.Kind != "acceptance" || a.Wait.Source != "actor" || a.Wait.MessageID != "report" {
		t.Fatalf("%+v", a)
	}
	file := filepath.Join(e.Root, run, "team.json")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		chat, err := runstore.ReadTeamContext(e.Root, run, runstore.TeamReadOptions{})
		if err != nil || chat.Room.Actors["developer"].Wait == nil {
			t.Fatal(chat, err)
		}
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || client.calls != 1 {
		t.Fatal("чтение меняет состояние или вызывает модель")
	}
	postAssignment(t, e, run, "followup")
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", now.Add(time.Hour)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	a = readChat(t, e, run).Room.Actors["developer"]
	if a.Wait != nil || a.Dependency != nil {
		t.Fatal("старый блокер остался в новом ходе", a)
	}
}

// Разрешение отличается от обычной ошибки; следующая ошибка не наследует
// устаревшую причину. Статус blocked и запрет неоднозначного retry сохранены.
func TestPermissionWaitThenError(t *testing.T) {
	e, run, client, _ := messageRoom(t)
	postAssignment(t, e, run, "permission-task")
	client.execute = func(_ context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "dev", "one")
		return codex.Result{Status: "running", TurnID: "one", TurnAttempted: true}, &codex.InteractionRequired{Event: codex.Event{Method: "item/commandExecution/requestApproval"}}
	}
	if err := e.Process(t.Context(), run, "developer"); err == nil {
		t.Fatal("потеряна ошибка взаимодействия")
	}
	a := readChat(t, e, run).Room.Actors["developer"]
	if a.Status != "blocked" || a.Wait.Kind != "permission" {
		t.Fatal(a)
	}
	if err := e.block(t.Context(), run, "developer", "История изменилась", true); err != nil {
		t.Fatal(err)
	}
	if a := readChat(t, e, run).Room.Actors["developer"]; a.Wait.Kind != "error" || a.ApprovalPending {
		t.Fatal(a)
	}
}
