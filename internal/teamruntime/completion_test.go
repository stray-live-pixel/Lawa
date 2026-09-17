package teamruntime

import (
	"context"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"strings"
	"testing"
	"time"
)

// Достижение является durable-решением Босса. Повтор RPC не дублирует событие,
// поздний Notify не стирает галочку, следующий таймер не запускает новую модель.
func TestCompleteGoalStopsTeam(t *testing.T) {
	e, run, client, now := teamEngine(t)
	var finalID string
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "boss-thread", "boss-turn")
		tool(t, c, "team_complete", `{"text":"@human Игра готова. Открой index.html. Проверки пройдены."}`)
		chat := readChat(t, e, run)
		finalID = chat.Messages[len(chat.Messages)-1].ID
		if chat.Room.AchievedAt == nil || chat.Messages[len(chat.Messages)-2].Kind != "achievement" || chat.Messages[len(chat.Messages)-1].To != "human" {
			t.Fatal(chat)
		}
		if err := c.Notify(codex.Event{Method: "item/completed", Params: []byte(`{"item":{"type":"agentMessage","text":"Завершаю ход"}}`)}); err != nil {
			t.Fatal(err)
		}
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "boss")
	chat := readChat(t, e, run)
	if chat.Room.Actors["boss"].Delivery != nil || !chat.Room.Actors["boss"].NextCheck.IsZero() || chat.Room.Actors["boss"].Summary != "" {
		t.Fatal(chat.Room)
	}
	if chat.History.Frames[0].AchievedAt != nil || chat.History.Frames[len(chat.History.Frames)-1].AchievedAt == nil {
		t.Fatal("потеряна история достижения")
	}
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", finalID, "@human Игра готова. Открой index.html. Проверки пройдены."); err != nil {
		t.Fatal(err)
	}
	if len(readChat(t, e, run).Messages) != 3 {
		t.Fatal("повторный финал")
	}
	*now = now.Add(24 * time.Hour)
	process(t, e, run, "boss")
	if client.calls != 1 {
		t.Fatal("агент проснулся после достижения")
	}
	if claimed, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || claimed {
		t.Fatal(claimed, err)
	}
	if _, err := runstore.PostTeam(t.Context(), e.Root, run, "", "after", "@boss Ещё вопрос"); err == nil {
		t.Fatal("чат не закрыт")
	}
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "developer", "fake", "@human Готово"); err == nil {
		t.Fatal("не проверена роль")
	}
}

// Решение нельзя принять одновременно с выполняющимся turn коллеги. Блокировка
// и выдача поручений используют один team.lock, поэтому поздний claim тоже безопасен.
func TestCompleteGoalWaitsForColleague(t *testing.T) {
	e, run, _, now := teamEngine(t)
	if _, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil {
		t.Fatal(err)
	}
	if err := runstore.SummonDeveloper(t.Context(), e.Root, run, "boss"); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "task", "@developer Сделай игру"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", now.Add(6*time.Minute)); err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	before := readChat(t, e, run)
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "done", "@human Готово"); err == nil || !strings.Contains(err.Error(), "дождись") {
		t.Fatal(err)
	}
	after := readChat(t, e, run)
	if after.Room.AchievedAt != nil || len(before.Messages) != len(after.Messages) {
		t.Fatal("частично записан финал")
	}
}

// У сохранённого Codex thread список tools неизменен. Явная команда через
// прежний team_post должна завершать тот же заказ, сохраняя личность и историю.
func TestCompleteLegacyTool(t *testing.T) {
	e, run, client, _ := teamEngine(t)
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "thread", "turn")
		tool(t, c, "team_post", `{"text":"/complete @human Игра проверена, открой index.html."}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "boss")
	chat := readChat(t, e, run)
	if chat.Room.AchievedAt == nil || chat.Messages[len(chat.Messages)-1].Text != "@human Игра проверена, открой index.html." {
		t.Fatal(chat)
	}
}

// Падение после durable-достижения, но до turn/completed не должно отправлять
// итог второй раз. После рестарта только Observer закрывает сохранённую доставку.
func TestCompletedGoalRecoversWithoutNewTurn(t *testing.T) {
	e, run, client, now := teamEngine(t)
	if _, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil {
		t.Fatal(err)
	}
	if err := e.change(t.Context(), run, "boss", func(a *runstore.TeamActor) {
		a.ThreadID = "thread"
		a.TurnID = "turn"
		a.Delivery.TurnID = "turn"
		a.Delivery.Attempted = true
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "done", "@human Проверено, готово."); err != nil {
		t.Fatal(err)
	}
	client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "turn", LatestTurnStatus: "completed"}
	process(t, e, run, "boss")
	c := readChat(t, e, run)
	if client.calls != 0 || c.Room.Actors["boss"].Delivery != nil || !c.Room.Actors["boss"].NextCheck.IsZero() || len(c.Messages) != 3 {
		t.Fatal(c.Room)
	}
}
