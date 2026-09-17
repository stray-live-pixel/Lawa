package teamruntime

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/coordinator"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// fakeClient проверяет реальную маршрутизацию dynamic tools без сетевого LLM,
// а управляемые часы позволяют проверить пять минут без ожидания в тестах.
type fakeClient struct {
	calls     int
	continued []string
	observed  codex.Observation
	execute   func(context.Context, codex.Command) (codex.Result, error)
}

func (f *fakeClient) Run(ctx context.Context, c codex.Command) (codex.Result, error) {
	f.calls++
	return f.execute(ctx, c)
}
func (f *fakeClient) Continue(ctx context.Context, id string, c codex.Command) (codex.Result, error) {
	f.continued = append(f.continued, id)
	return f.Run(ctx, c)
}
func (f *fakeClient) OpenObserver(context.Context, string) (coordinator.Observer, error) {
	return &fakeObserver{f.observed}, nil
}

type fakeObserver struct{ observation codex.Observation }

func (f *fakeObserver) Inspect(string) (codex.Observation, error) { return f.observation, nil }
func (f *fakeObserver) Close() error                              { return nil }

func teamEngine(t *testing.T) (*Engine, string, *fakeClient, *time.Time) {
	t.Helper()
	root := t.TempDir()
	s, err := runstore.Create(root, runstore.Input{Order: true, Team: true, CWD: t.TempDir(), Task: "Создать платформер", WorkflowJSON: []byte(`{"id":"office","characters":{"boss":{"name":"Босс","history":"Опытный инженер","instructions":"Веди команду"}},"steps":[{"id":"boss","type":"agent","character":"boss","prompt":"Работай","dependsOn":[]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	client := &fakeClient{}
	return &Engine{Root: root, Client: client, Now: func() time.Time { return now }}, s.Meta.RunID, client, &now
}
func readChat(t *testing.T, e *Engine, run string) runstore.TeamChat {
	t.Helper()
	c, err := runstore.ReadTeam(e.Root, run)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func process(t *testing.T, e *Engine, run, id string) {
	t.Helper()
	if err := e.Process(t.Context(), run, id); err != nil {
		t.Fatal(err)
	}
}
func postHuman(t *testing.T, e *Engine, run, id, text string) {
	t.Helper()
	if _, err := runstore.PostTeam(t.Context(), e.Root, run, "", id, text); err != nil {
		t.Fatal(err)
	}
}
func tool(t *testing.T, c codex.Command, name, args string) {
	t.Helper()
	if _, err := c.CallDynamicTool(t.Context(), codex.DynamicToolCall{Tool: name, CallID: name + args, Arguments: []byte(args), ThreadID: c.Title, TurnID: c.Text}); err != nil {
		t.Fatal(err)
	}
}
func start(t *testing.T, c codex.Command, thread, turn string) {
	t.Helper()
	if err := c.OnThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := c.OnTurn(turn, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// Первый ход немедленный; последующие — лишь после личного покоя. Сообщение,
// пришедшее во время turn, не теряется при продвижении курсора текущей порции.
func TestTeamLifecycleAndHumanRelay(t *testing.T) {
	e, run, client, now := teamEngine(t)
	if c := readChat(t, e, run); len(c.Room.Actors) != 1 || c.Room.Actors["boss"] == nil {
		t.Fatal(c.Room)
	}
	postHuman(t, e, run, "note", "В тексте @developer не является адресом")
	if _, err := runstore.PostTeam(t.Context(), e.Root, run, "", "early", "@developer Сделай игру"); err == nil {
		t.Fatal("поручение отсутствующему сотруднику")
	}
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		if c.Permissions == nil || len(c.Permissions.WritePaths) != 1 || c.Permissions.WritePaths[0] != c.CWD {
			t.Fatal("команда должна иметь доступ записи только к выбранному проекту", c.Permissions)
		}
		start(t, c, "boss-thread", "boss-1")
		tool(t, c, "team_summon", `{"id":"developer"}`)
		tool(t, c, "team_summon", `{"id":"developer"}`)
		tool(t, c, "team_post", `{"text":"@developer Реализуй платформер"}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "boss")
	chat := readChat(t, e, run)
	if len(chat.Room.Actors) != 2 || chat.Room.Actors["boss"].Status != "monitoring" {
		t.Fatal(chat.Room)
	}
	systems := 0
	for _, m := range chat.Messages {
		if m.Kind == "system" {
			systems++
		}
	}
	if systems != 1 {
		t.Fatal("неидемпотентный призыв")
	}
	process(t, e, run, "developer")
	if client.calls != 1 {
		t.Fatal("разработчик запущен раньше таймера")
	}
	postHuman(t, e, run, "direct", "@developer Добавь прыжок")
	*now = chat.Room.Actors["developer"].NextCheck
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "dev-thread", "dev-1")
		if _, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{Tool: "team_post", CallID: "forbidden", Arguments: []byte(`{"text":"@human Готово"}`)}); err == nil {
			t.Fatal("прямой ответ Челу")
		}
		if _, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{Tool: "team_summon", Arguments: []byte(`{"id":"developer"}`)}); err == nil {
			t.Fatal("призыв не Боссом")
		}
		postHuman(t, e, run, "during", "@developer Проверь клавиатуру")
		tool(t, c, "team_post", `{"text":"@boss Платформер и прыжок готовы. Передай Челу."}`)
		*now = now.Add(2 * time.Minute)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "developer")
	chat = readChat(t, e, run)
	if got := chat.Room.Actors["developer"].NextCheck; !got.Equal(now.Add(5 * time.Minute)) {
		t.Fatal("таймер не от завершения", got)
	}
	last := chat.Messages[len(chat.Messages)-1]
	if last.To != "boss" || last.ReplyTo != "direct" {
		t.Fatal(last)
	}
	process(t, e, run, "developer")
	if client.calls != 2 {
		t.Fatal("turn во время личного таймера")
	}
	*now = chat.Room.Actors["developer"].NextCheck
	client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "dev-1", LatestTurnStatus: "completed"}
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "dev-thread", "dev-2")
		tool(t, c, "team_post", `{"text":"@boss Клавиатура проверена"}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "developer")
	if len(client.continued) != 1 || client.continued[0] != "dev-thread" {
		t.Fatal("потеряна история личности", client.continued)
	}
	*now = now.Add(5 * time.Minute)
	process(t, e, run, "developer")
	if client.calls != 3 {
		t.Fatal("пустая очередь запустила модель")
	}
}

// Старый turn личности нельзя принять за результат новой доставки. Только
// подтверждённый ID именно текущей порции позволяет восстановить завершение.
func TestRecoveryDoesNotRepeatDelivery(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			e, run, client, now := teamEngine(t)
			_, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now)
			if err != nil {
				t.Fatal(err)
			}
			err = e.change(t.Context(), run, "boss", func(a *runstore.TeamActor) {
				a.ThreadID = "thread"
				a.TurnID = "old"
				a.Delivery.Attempted = true
				if confirmed {
					a.Delivery.TurnID = "current"
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "current", LatestTurnStatus: "completed"}
			process(t, e, run, "boss")
			a := readChat(t, e, run).Room.Actors["boss"]
			if confirmed && (a.Delivery != nil || a.Cursor != 1) {
				t.Fatal(a)
			}
			if !confirmed && (a.Status != "blocked" || a.Cursor != 0) {
				t.Fatal(a)
			}
			if client.calls != 0 {
				t.Fatal("слепой повтор")
			}
		})
	}
}

// Ошибка до отправки допускает явный retry. Неизвестный результат сети — нет.
func TestRetryOnlyBeforeDispatch(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(fmt.Sprint(attempted), func(t *testing.T) {
			e, run, client, _ := teamEngine(t)
			client.execute = func(context.Context, codex.Command) (codex.Result, error) {
				return codex.Result{CreationAttempted: attempted}, errors.New("сбой запуска")
			}
			if err := e.Process(t.Context(), run, "boss"); err == nil {
				t.Fatal("скрыта ошибка")
			}
			a := readChat(t, e, run).Room.Actors["boss"]
			if a.Status != "blocked" || a.Delivery.Attempted != attempted {
				t.Fatal(a)
			}
			err := RetryUnsent(t.Context(), e.Root, run, "boss")
			if (err != nil) != attempted {
				t.Fatal("неверный retry", err)
			}
		})
	}
}

// Отмена сервера передаётся исходному turn; никакой второй writer не открывается.
func TestCancellationInterruptsOwnedTurn(t *testing.T) {
	e, run, client, _ := teamEngine(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var interrupted atomic.Bool
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		if err := c.OnThread("thread"); err != nil {
			return codex.Result{}, err
		}
		if err := c.OnTurn("turn", func(context.Context) error { interrupted.Store(true); return nil }); err != nil {
			return codex.Result{}, err
		}
		cancel()
		<-ctx.Done()
		return codex.Result{TurnAttempted: true, TurnID: "turn", Status: "interrupted"}, ctx.Err()
	}
	_ = e.Process(ctx, run, "boss")
	if !interrupted.Load() || readChat(t, e, run).Room.Actors["boss"].Status != "blocked" {
		t.Fatal("turn не остановлен")
	}
}

// Два сервера не могут параллельно владеть одним сотрудником.
func TestActorLeaseExcludesSecondServer(t *testing.T) {
	e, run, client, _ := teamEngine(t)
	lock, err := runstore.LockTeamActor(e.Root, run, "boss")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = e.Process(t.Context(), run, "boss"); err == nil || client.calls != 0 {
		t.Fatal("двойной владелец", err)
	}
}

// Удаление старым dashboard не должно стереть очередь живой команды: её actor
// lock независим от coordinator.lock. После завершения idle-заказ удалим.
func TestRemoveProtectsActiveRoom(t *testing.T) {
	e, run, _, now := teamEngine(t)
	lock, err := runstore.LockTeamActor(e.Root, run, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if err = runstore.Remove(e.Root, run); err == nil {
		t.Fatal("удалён работающий сотрудник")
	}
	lock.Close()
	if _, err = runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil {
		t.Fatal(err)
	}
	if err = runstore.Remove(e.Root, run); err == nil {
		t.Fatal("удалена неподтверждённая доставка")
	}
	if err = e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	if err = runstore.Remove(e.Root, run); err != nil {
		t.Fatal(err)
	}
}

// Внешнее продолжение thread не должно смешать контекст личности с новой
// порцией команды. Очередь сохраняется, сетевой Continue не вызывается.
func TestContinueRejectsForeignTurn(t *testing.T) {
	e, run, client, _ := teamEngine(t)
	if err := e.change(t.Context(), run, "boss", func(a *runstore.TeamActor) { a.ThreadID = "thread"; a.TurnID = "ours" }); err != nil {
		t.Fatal(err)
	}
	client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "foreign", LatestTurnStatus: "completed"}
	process(t, e, run, "boss")
	a := readChat(t, e, run).Room.Actors["boss"]
	if client.calls != 0 || a.Status != "blocked" || a.Cursor != 0 || !a.Delivery.Attempted {
		t.Fatal("чужой turn принят", a)
	}
}

// Порча записи не должна превращаться в panic фонового обхода всех команд.
func TestCorruptActorIsRejected(t *testing.T) {
	e, run, _, _ := teamEngine(t)
	if err := runstore.UpdateTeam(t.Context(), e.Root, run, func(chat *runstore.TeamChat) error { chat.Room.Actors["developer"] = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.ReadTeam(e.Root, run); err == nil {
		t.Fatal("принят null-сотрудник")
	}
}

// Облачко ограничено публичным действием: сырые рассуждения и секретные
// аргументы инструментов не превращаются в общий текст.
func TestActivityUsesOnlyPublicSummary(t *testing.T) {
	e, run, _, _ := teamEngine(t)
	for _, event := range []codex.Event{
		{Method: "item/started", Params: []byte(`{"item":{"type":"commandExecution","command":"секретная команда"}}`)},
		{Method: "item/reasoning/summaryTextDelta", Params: []byte(`{"delta":"приватное"}`)},
	} {
		if err := e.activity(run, "boss", event); err != nil {
			t.Fatal(err)
		}
	}
	if got := readChat(t, e, run).Room.Actors["boss"].Summary; got != "Работает с проектом" {
		t.Fatal(got)
	}
}
