package teamruntime

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/capacity"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// messageRoom оставляет Босса в активном ходе под actor lock: он может выдавать
// поручения, но scheduler не попытается восстановить его тестовую доставку.
func messageRoom(t *testing.T) (*Engine, string, *fakeClient, *time.Time) {
	t.Helper()
	e, run, client, now := teamEngine(t)
	lock, err := runstore.LockTeamActor(e.Root, run, "boss")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := runstore.SummonActor(t.Context(), e.Root, run, "boss", "developer"); err != nil {
		t.Fatal(err)
	}
	return e, run, client, now
}

// postAssignment использует настоящую маршрутизацию и запись на диск.
func postAssignment(t *testing.T, e *Engine, run, id string) {
	t.Helper()
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", id, "@developer Проверь "+id); err != nil {
		t.Fatal(err)
	}
}

// Сохранённое пробуждение переживает новый Engine. Занятый общий слот не
// расходует очередь; после освобождения ближайший тик забирает всю порцию.
func TestMessageWakeupRestartAndCapacity(t *testing.T) {
	e, run, client, now := messageRoom(t)
	postAssignment(t, e, run, "first")
	postAssignment(t, e, run, "second")
	e = &Engine{Root: e.Root, Client: client, Now: func() time.Time { return *now }}
	var err error
	e.Pool, err = capacity.Configure(e.Root, "1")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := e.Pool.TryAcquire()
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	defer lease.Release()
	e.tick(t.Context())
	e.wg.Wait()
	if client.calls != 0 || readChat(t, e, run).Room.Actors["developer"].Delivery != nil {
		t.Fatal("исчерпанный лимит не остановил запуск")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	client.execute = func(_ context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "dev", "one")
		ids := readChat(t, e, run).Room.Actors["developer"].Delivery.IDs
		if !reflect.DeepEqual(ids, []string{"first", "second"}) {
			t.Errorf("потеряна порция: %v", ids)
		}
		return codex.Result{Status: "completed"}, nil
	}
	e.tick(t.Context())
	e.wg.Wait()
	if client.calls != 1 {
		t.Fatal("поручение не запущено ближайшим тиком", client.calls)
	}
	before := readChat(t, e, run).Room.Actors["developer"].NextCheck
	postAssignment(t, e, run, "first")
	if after := readChat(t, e, run).Room.Actors["developer"].NextCheck; !after.Equal(before) {
		t.Fatal("повтор события разбудил агента")
	}
	*now = now.Add(10 * time.Minute)
	e.tick(t.Context())
	e.wg.Wait()
	if client.calls != 1 {
		t.Fatal("пустая очередь вызвала модель")
	}
}

// Сообщения внутри хода остаются за End. Тик и второй Process не создают
// второй turn; после завершения оба сообщения попадают в единственное Continue.
func TestMessagesDuringTurnAreBatched(t *testing.T) {
	e, run, client, _ := messageRoom(t)
	postAssignment(t, e, run, "first")
	client.execute = func(_ context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "dev", "one")
		postAssignment(t, e, run, "second")
		postAssignment(t, e, run, "third")
		postAssignment(t, e, run, "second")
		e.tick(t.Context())
		if err := e.Process(t.Context(), run, "developer"); err == nil {
			t.Error("второй владелец личности")
		}
		if ids := readChat(t, e, run).Room.Actors["developer"].Delivery.IDs; !reflect.DeepEqual(ids, []string{"first"}) {
			t.Errorf("изменена активная порция: %v", ids)
		}
		return codex.Result{Status: "completed"}, nil
	}
	e.tick(t.Context())
	e.wg.Wait()
	client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "one", LatestTurnStatus: "completed"}
	client.execute = func(_ context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "dev", "two")
		if ids := readChat(t, e, run).Room.Actors["developer"].Delivery.IDs; !reflect.DeepEqual(ids, []string{"second", "third"}) {
			t.Errorf("потеряна следующая порция: %v", ids)
		}
		return codex.Result{Status: "completed"}, nil
	}
	e.tick(t.Context())
	e.wg.Wait()
	if client.calls != 2 || len(client.continued) != 1 {
		t.Fatal(client.calls, client.continued)
	}
}

// Восстановление завершённого turn тоже будит сохранённый хвост; неизвестный
// turn блокирует доставку, и новое поручение не снимает эту защиту.
func TestRecoveryWithPendingMessages(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ambiguous", true: "confirmed"}[confirmed], func(t *testing.T) {
			e, run, client, now := messageRoom(t)
			postAssignment(t, e, run, "first")
			if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", *now); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if err := e.change(t.Context(), run, "developer", func(a *runstore.TeamActor) {
				a.ThreadID = "dev"
				a.TurnID = "one"
				a.Delivery.Attempted = true
				if confirmed {
					a.Delivery.TurnID = "one"
				}
			}); err != nil {
				t.Fatal(err)
			}
			postAssignment(t, e, run, "second")
			e = &Engine{Root: e.Root, Client: client, Now: func() time.Time { return *now }}
			client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "one", LatestTurnStatus: "completed"}
			process(t, e, run, "developer")
			a := readChat(t, e, run).Room.Actors["developer"]
			if confirmed {
				if a.Delivery != nil || !a.NextCheck.Equal(*now) {
					t.Fatal("хвост не готов после восстановления", a)
				}
			} else {
				postAssignment(t, e, run, "third")
				process(t, e, run, "developer")
				a = readChat(t, e, run).Room.Actors["developer"]
				if a.Status != "blocked" || a.Delivery == nil || !reflect.DeepEqual(a.Delivery.IDs, []string{"first"}) {
					t.Fatal("неоднозначная доставка потеряна", a)
				}
			}
			if client.calls != 0 {
				t.Fatal("восстановление повторило запрос")
			}
		})
	}
}

// Достигнутая цель останавливает scheduler. Позднее поручение сотруднику
// отклоняется и не возобновляет комнату.
func TestAddressedMessageDoesNotWakeCompletedTeam(t *testing.T) {
	e, run, client, now := messageRoom(t)
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "done", "@human Проверено, готово."); err != nil {
		t.Fatal(err)
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "late", "@developer Ещё задача"); err == nil {
		t.Fatal("новое поручение после завершения принято")
	}
	*now = now.Add(10 * time.Minute)
	e.tick(t.Context())
	e.wg.Wait()
	if client.calls != 0 || readChat(t, e, run).Room.AchievedAt == nil {
		t.Fatal("завершённая команда возобновилась")
	}
}
