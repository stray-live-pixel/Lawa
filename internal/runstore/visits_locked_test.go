//go:build darwin || linux

package runstore

import (
	"errors"
	"os"
	"reflect"
	"syscall"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// TestVisitLifecycle проверяет адресацию только по visitId, пакетный reserve,
// счётчик distinct turns, безопасную terminal-диагностику и неизменяемость финала.
func TestVisitLifecycle(t *testing.T) {
	root, initial, run := testAgentGraphRun(t)
	first, second := initial.Meta.Visits[0].VisitID, initial.Meta.Visits[1].VisitID
	if err := run.Reserve([]string{"choice"}); err == nil {
		t.Fatal("legacy Reserve неожиданно управляет v4")
	}
	for _, invalid := range [][]string{{first, "missing"}, {first, first}} {
		if err := run.ReserveVisits(invalid); err == nil {
			t.Fatalf("принята неверная волна %v", invalid)
		}
	}
	if err := run.ReserveVisits([]string{first}); err != nil {
		t.Fatal(err)
	}
	if err := run.ReleaseUnattemptedVisit(first); err != nil {
		t.Fatal(err)
	}
	if err := run.ReserveVisits([]string{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(first, scheduler.Unknown, "chat-choice", ""); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []string{"turn-1", "turn-1", "turn-2"} {
		if err := run.SetVisitTurn(first, turn); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.UpdateVisit(first, scheduler.Running, "chat-choice", ""); err != nil {
		t.Fatal(err)
	}
	decision, err := run.CommitDecision(first, "chat-choice", "turn-2", "go", "нужна работа", "call-1")
	if err != nil || !reflect.DeepEqual(decision.To, []string{"work"}) || !reflect.DeepEqual(decision.Skipped, []string{"done", "fail"}) {
		t.Fatalf("маршрут не материализован: %+v, %v", decision, err)
	}
	if err := run.SetVisitTurn(first, "turn-3"); err == nil {
		t.Fatal("посещение с durable decision приняло новый turn")
	}
	if err := run.UpdateVisit(first, scheduler.Cancelled, "chat-choice", ""); err != nil {
		t.Fatal(err)
	}
	if err := run.SetVisitTurn(first, "turn-3"); err != nil {
		t.Fatalf("Cancelled с неприменённым решением не продолжился: %v", err)
	}
	if err := run.UpdateVisit(first, scheduler.Running, "chat-choice", ""); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(first, scheduler.Succeeded, "chat-choice", ""); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(first, scheduler.Running, "chat-choice", ""); err == nil || run.SetVisitTurn(first, "turn-4") == nil {
		t.Fatal("terminal visit изменён")
	}
	if err := run.UpdateVisit(second, scheduler.Unknown, "chat-audit", ""); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(second, scheduler.Cancelled, "chat-audit", ""); err != nil {
		t.Fatal(err)
	}
	if err := run.SetVisitTurn(second, "turn-audit"); err != nil {
		t.Fatalf("Cancelled должен продолжаться новым turn: %v", err)
	}
	if err := run.UpdateVisit(second, scheduler.Running, "chat-audit", ""); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(second, scheduler.Failed, "chat-audit", "сеть недоступна"); err != nil {
		t.Fatal(err)
	}
	got, err := run.Load()
	if err != nil || got.Meta.Visits[0].Attempt != 3 || got.Meta.Visits[0].TurnID != "turn-3" || got.Meta.Visits[0].Decision.TurnID != "turn-2" || got.Meta.Visits[1].TechnicalError != "сеть недоступна" {
		t.Fatalf("жизненный цикл потерян: %+v, %v", got.Meta.Visits, err)
	}
	if err := RemoveUnstarted(root, initial.Meta.RunID); err == nil {
		t.Fatal("начатый v4 run удалён")
	}
}

// TestCommitDecisionIdempotency проверяет точный replay и poison marker второго
// противоречивого вызова. Возвращённая копия не даёт вызывающему изменить диск.
func TestCommitDecisionIdempotency(t *testing.T) {
	_, initial, run := testAgentGraphRun(t)
	visitID := initial.Meta.Visits[0].VisitID
	err := run.ReserveVisits([]string{visitID})
	if err == nil {
		err = run.UpdateVisit(visitID, scheduler.Unknown, "chat", "")
	}
	if err == nil {
		err = run.SetVisitTurn(visitID, "turn")
	}
	if err != nil {
		t.Fatal(err)
	}
	before, _ := run.Load()
	for _, invalid := range [][3]string{{"other", "turn", "go"}, {"chat", "other", "go"}, {"chat", "turn", "missing"}} {
		if _, err := run.CommitDecision(visitID, invalid[0], invalid[1], invalid[2], "", "call"); err == nil {
			t.Fatalf("принята неверная связь решения: %v", invalid)
		}
	}
	if got, _ := run.Load(); !reflect.DeepEqual(got, before) {
		t.Fatal("отказ решения изменил snapshot")
	}
	record, err := run.CommitDecision(visitID, "chat", "turn", "go", "пояснение", "call-1")
	if err != nil {
		t.Fatal(err)
	}
	record.To[0] = "подмена"
	if _, err := run.CommitDecision(visitID, "chat", "turn", "go", "пояснение", "call-1"); err != nil {
		t.Fatalf("точный replay не идемпотентен: %v", err)
	}
	if _, err := run.CommitDecision(visitID, "chat", "turn", "missing", "другое", "call-2"); err == nil {
		t.Fatal("второе противоречивое решение принято")
	}
	stored, err := run.Load()
	if err != nil || stored.Meta.Visits[0].Decision.Key != "go" || stored.Meta.Visits[0].Decision.To[0] != "work" || stored.Meta.Visits[0].Decision.Error == "" {
		t.Fatalf("первый маршрут или poison marker потерян: %+v, %v", stored.Meta.Visits[0].Decision, err)
	}
	if err := run.UpdateVisit(visitID, scheduler.Succeeded, "chat", ""); err != nil {
		t.Fatalf("технический результат poisoned turn не сохранён: %v", err)
	}
	stored, err = run.Load()
	if err != nil || stored.Meta.Visits[0].State != scheduler.Succeeded || stored.Meta.Visits[0].Decision.Error == "" || stored.Meta.Visits[0].Decision.Applied {
		t.Fatalf("poisoned решение ошибочно применено или потеряно: %+v, %v", stored.Meta.Visits[0], err)
	}

	// Успешный terminal turn без единого вызова choose_decision также остаётся
	// фактом истории. Следующий планировщик превратит его в failed всего run и
	// не будет притворяться, что транспортный turn завершился ошибкой.
	_, missingInitial, missingRun := testAgentGraphRun(t)
	missingID := missingInitial.Meta.Visits[0].VisitID
	if err := missingRun.ReserveVisits([]string{missingID}); err == nil {
		err = missingRun.UpdateVisit(missingID, scheduler.Unknown, "chat-missing", "")
	}
	if err == nil {
		err = missingRun.SetVisitTurn(missingID, "turn-missing")
	}
	if err == nil {
		err = missingRun.UpdateVisit(missingID, scheduler.Succeeded, "chat-missing", "")
	}
	if err != nil {
		t.Fatalf("completed turn без решения не сохранён: %v", err)
	}
}

// TestTerminalRunDoesNotReserve не позволяет гонке с параллельной веткой
// запустить новый thread после durable commit глобального finish.
func TestTerminalRunDoesNotReserve(t *testing.T) {
	_, initial, run := testAgentGraphRun(t)
	choiceID, pendingID := initial.Meta.Visits[0].VisitID, initial.Meta.Visits[1].VisitID
	err := run.mutateVisits((*os.File).Sync, func(snapshot *Snapshot) (bool, error) {
		choice := &snapshot.Meta.Visits[0]
		choice.State, choice.CodexThreadID, choice.TurnID, choice.Attempt = scheduler.Succeeded, "chat", "turn", 1
		choice.Decision = &DecisionRecord{Key: "done", TurnID: "turn", CallID: "call", Finish: cloneOutcome(snapshot.Workflow.Steps[0].Decisions["done"].Finish), Skipped: []string{"fail", "go"}, Applied: true}
		snapshot.Meta.RunState, snapshot.Meta.StopReason, snapshot.Meta.StopVisitID = RunSucceeded, "агент выбрал успешное завершение", choice.VisitID
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.ReserveVisits([]string{pendingID}); err == nil {
		t.Fatalf("terminal run зарезервировал посещение после finish %s", choiceID)
	}
	if err := run.UpdateVisit(pendingID, scheduler.Starting, "", ""); err == nil {
		t.Fatal("terminal run перевёл Pending в Starting через UpdateVisit")
	}
	if err := run.mutateVisits((*os.File).Sync, func(snapshot *Snapshot) (bool, error) {
		visit := &snapshot.Meta.Visits[1]
		visit.State, visit.CodexThreadID, visit.TurnID, visit.Attempt = scheduler.Running, "chat-pending", "turn-pending", 1
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.CommitDecision(pendingID, "chat-pending", "turn-pending", "done", "поздний выбор", "call-pending"); err == nil {
		t.Fatal("terminal run принял первое решение параллельного агента")
	}
	if err := run.mutateVisits((*os.File).Sync, func(snapshot *Snapshot) (bool, error) {
		snapshot.Meta.Visits[1].State = scheduler.Cancelled
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.SetVisitTurn(pendingID, "turn-after-finish"); err == nil {
		t.Fatal("terminal run продолжил Cancelled новым turn")
	}
}

// TestVisitUpdateSyncFailure воспроизводит отказ до Rename. Владелец после него
// останавливается, а повторное открытие видит целую прежнюю metadata и продолжает.
func TestVisitUpdateSyncFailure(t *testing.T) {
	root, initial, run := testAgentGraphRun(t)
	visitID := initial.Meta.Visits[0].VisitID
	if err := run.ReserveVisits([]string{visitID}); err != nil {
		t.Fatal(err)
	}
	err := run.updateVisit(visitID, scheduler.Unknown, "chat", "", func(file *os.File) error {
		info, statErr := file.Stat()
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() {
			return syscall.EIO
		}
		return file.Sync()
	})
	if !errors.Is(err, syscall.EIO) || !errors.Is(run.ReserveVisits(nil), syscall.EIO) {
		t.Fatalf("ошибка записи не остановила владельца: %v", err)
	}
	external, err := Load(root, initial.Meta.RunID)
	if err != nil || external.Meta.Visits[0].State != scheduler.Starting || external.Meta.Visits[0].CodexThreadID != "" {
		t.Fatalf("отказ до Rename повредил metadata: %+v, %v", external.Meta.Visits, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.UpdateVisit(visitID, scheduler.Unknown, "chat", ""); err != nil {
		t.Fatalf("повторное открытие не восстановило запись: %v", err)
	}
}
