package teamruntime

import (
	"context"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// Прямой запрос Чела разработчику после завершения получает два адресата,
// Босс может общаться с Челом сразу; достижение ждёт отчёта и приёмки.
func TestHumanReopensDeveloperAndBoss(t *testing.T) {
	e, run, _, now := teamEngine(t)
	claim := func(id string) {
		t.Helper()
		ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, id, *now)
		if err != nil || !ok {
			t.Fatal(id, ok, err)
		}
	}
	claim("boss")
	if err := runstore.SummonDeveloper(t.Context(), e.Root, run, "boss"); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "finish", "@human Игра готова."); err != nil {
		t.Fatal(err)
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	before := readChat(t, e, run)
	postHuman(t, e, run, "question", "@developer Как устроен прыжок?")
	reopened := readChat(t, e, run)
	if reopened.Room.AchievedAt != nil || !reopened.Messages[len(reopened.Messages)-1].NotifyBoss {
		t.Fatal("не возобновлены оба сотрудника")
	}
	count := len(reopened.Messages)
	postHuman(t, e, run, "question", "@developer Как устроен прыжок?")
	if len(readChat(t, e, run).Messages) != count {
		t.Fatal("повтор создал событие")
	}
	// Запоздалый retry старого завершения не должен закрыть новый вопрос.
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "finish", "@human Игра готова."); err != nil {
		t.Fatal(err)
	}
	if readChat(t, e, run).Room.AchievedAt != nil {
		t.Fatal("старый retry закрыл новую работу")
	}
	postHuman(t, e, run, "question-2", "@developer И какая сила гравитации?")
	claim("boss")
	claim("developer")
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "early", "@human Разработчик проверяет прыжок"); err != nil {
		t.Fatal("сообщение о ходе работы запрещено", err)
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "developer", "wrong", "@human Ответ напрямую"); err == nil {
		t.Fatal("обход Босса")
	}
	reply, err := runstore.PostActor(t.Context(), e.Root, run, "developer", "answer", "@boss Прыжок задаётся скоростью и гравитацией.")
	if err != nil || reply.ReplyTo != "question-2" || len(reply.ReplyToIDs) != 2 {
		t.Fatal(reply, err)
	}
	if err := e.finish(t.Context(), run, "developer"); err != nil {
		t.Fatal(err)
	}
	claim("boss")
	if _, err := runstore.AcceptTeamTasks(t.Context(), e.Root, run, "boss", "accepted", []string{"question", "question-2"}, "answer", "Сверил объяснение прыжка с кодом"); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "finish-again", "@human Разработчик подтвердил: прыжок задаётся скоростью и гравитацией."); err != nil {
		t.Fatal(err)
	}
	after := readChat(t, e, run)
	if after.Room.AchievedAt == nil || len(after.History.Frames) <= len(before.History.Frames) {
		t.Fatal("потеряна история возобновления")
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	// Обращение только к Боссу не возвращает личный таймер Разработчика.
	postHuman(t, e, run, "boss-only", "@boss Где инструкция?")
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", now.Add(time.Hour)); ok || err != nil {
		t.Fatal(ok, err)
	}
	claim("boss")
}

// Новое обращение во время завершающего turn остаётся в очереди, даже если
// прежняя цель уже успела получить галочку. Доставка не сбрасывается досрочно.
func TestHumanFollowupDuringFinish(t *testing.T) {
	e, run, _, now := teamEngine(t)
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "done", "@human Готово."); err != nil {
		t.Fatal(err)
	}
	postHuman(t, e, run, "new", "@boss Добавь второй уровень")
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", now.Add(time.Second)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	c := readChat(t, e, run)
	if len(c.Room.Actors["boss"].Delivery.IDs) != 1 || c.Room.Actors["boss"].Delivery.IDs[0] != "new" {
		t.Fatal(c.Room)
	}
	oldGoal := c.Goal
	if _, err := runstore.SetTeamGoal(t.Context(), e.Root, run, "boss", "goal", "Добавить второй уровень"); err != nil {
		t.Fatal(err)
	}
	c = readChat(t, e, run)
	if c.Goal != "Добавить второй уровень" || c.History.Frames[0].Goal != oldGoal {
		t.Fatal("новая цель изменила прошлое")
	}
}

// Старый thread не получает новые tools при resume: смена цели доступна
// через прежний team_post, с теми же правами и идемпотентностью.
func TestSetGoalLegacyTool(t *testing.T) {
	e, run, client, _ := teamEngine(t)
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "boss-thread", "goal-turn")
		tool(t, c, "team_post", `{"text":"/goal Добавить второй уровень"}`)
		tool(t, c, "team_post", `{"text":"/goal Добавить второй уровень"}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "boss")
	chat := readChat(t, e, run)
	if chat.Goal != "Добавить второй уровень" || len(chat.Messages) != 2 {
		t.Fatal(chat.Goal, chat.Messages)
	}
	if _, err := runstore.SetTeamGoal(t.Context(), e.Root, run, "developer", "forbidden", "Подмена"); err == nil {
		t.Fatal("Разработчик изменил цель")
	}
}

// Босс может прочесть общий чат раньше claim Разработчика. Чтение не даёт
// права поглотить адресованное коллеге обращение при завершении цели.
func TestCompletionPreservesUnclaimedHumanQuestion(t *testing.T) {
	e, run, _, now := teamEngine(t)
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := runstore.SummonDeveloper(t.Context(), e.Root, run, "boss"); err != nil {
		t.Fatal(err)
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	postHuman(t, e, run, "question", "@developer Объясни прыжок")
	postHuman(t, e, run, "boss-question", "@boss Подведи итог")
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "premature", "@human Готово"); err == nil {
		t.Fatal("потерян вопрос до claim коллеги")
	}
}
