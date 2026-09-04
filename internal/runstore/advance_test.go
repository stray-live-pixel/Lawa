//go:build darwin || linux

package runstore

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// advanceInput даёт fanout из одного решения и общий after-barrier. Второй
// стартовый visit нужен для проверки, что независимая активная работа не мешает
// применить route или немедленный explicit finish.
func advanceInput(t *testing.T) Input {
	t.Helper()
	return Input{WorkflowJSON: []byte(`{
  "version":2,
  "id":"advance",
  "start":["decision","parallel"],
  "steps":[
    {"id":"decision","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "branch":{"to":["left","right"]},"done":{"finish":"succeeded"},"fail":{"finish":"failed"}}},
    {"id":"parallel","type":"agent","prompt":"Параллельно","after":[]},
    {"id":"left","type":"agent","prompt":"Левая ветка","after":[]},
    {"id":"right","type":"agent","prompt":"Правая ветка","after":[]},
    {"id":"join","type":"agent","prompt":"Объединить","after":["left","right"]}
  ]
}`), Task: "Продвинуть граф", Comment: "Тест persistence bridge", CWD: t.TempDir()}
}

// skippedJoinInput воспроизводит обычный if/else с общим after-join. Выбранная
// ветка должна запустить агента, невыбранная — создать durable Skipped token,
// чтобы join не потерял одну половину барьера и не завершил run преждевременно.
func skippedJoinInput(t *testing.T) Input {
	t.Helper()
	return Input{WorkflowJSON: []byte(`{
  "version":2,
  "id":"skipped-join",
  "start":["choice"],
  "steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "left":{"to":["left"]},"right":{"to":["right"]}}},
    {"id":"left","type":"agent","prompt":"Левая ветка","after":[]},
    {"id":"right","type":"agent","prompt":"Правая ветка","after":[]},
    {"id":"join","type":"agent","prompt":"Объедини","after":["left","right"]}
  ]
}`), Task: "Проверить пропущенную ветку", CWD: t.TempDir()}
}

// nestedSkippedInput воспроизводит вложенную развилку из замечания ревью.
// Невыбранный outer-route ведёт не прямо к листу, а к ещё одному decision:
// такой кубик не должен выбирать бизнес-ключ, но обязан распространить ту же
// причинную волну сразу по объединению всех своих статических routes.
func nestedSkippedInput(t *testing.T) Input {
	t.Helper()
	return Input{WorkflowJSON: []byte(`{
  "version":2,
  "id":"nested-skipped",
  "start":["outer"],
  "steps":[
    {"id":"outer","type":"agent","prompt":"Выбери внешнюю ветку","after":[],"decisions":{
      "nested":{"to":["inner"]},"right":{"to":["right"]}}},
    {"id":"inner","type":"agent","prompt":"Выбери внутреннюю ветку","after":[],"decisions":{
      "a":{"to":["leaf-a"]},"b":{"to":["leaf-b"]}}},
    {"id":"right","type":"agent","prompt":"Правая ветка","after":[]},
    {"id":"leaf-a","type":"agent","prompt":"Лист A","after":[]},
    {"id":"leaf-b","type":"agent","prompt":"Лист B","after":[]},
    {"id":"join","type":"agent","prompt":"Объедини","after":["right","leaf-a","leaf-b"]}
  ]
}`), Task: "Проверить вложенную пропущенную ветку", CWD: t.TempDir()}
}

// terminalNestedSkippedInput отделяет критичную границу explicit finish. После
// публикации terminal run продвигать нельзя, поэтому весь transitive skip-chain
// должен получить настоящие visitId и попасть в тот же metadata commit.
func terminalNestedSkippedInput(t *testing.T) Input {
	t.Helper()
	return Input{WorkflowJSON: []byte(`{
  "version":2,
  "id":"terminal-nested-skipped",
  "start":["choice"],
  "steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "investigate":{"to":["inner"]},"safe":{"finish":"succeeded"}}},
    {"id":"inner","type":"agent","prompt":"Уточни","after":[],"decisions":{
      "a":{"to":["leaf-a"]},"b":{"to":["leaf-b"]}}},
    {"id":"leaf-a","type":"agent","prompt":"Лист A","after":[]},
    {"id":"leaf-b","type":"agent","prompt":"Лист B","after":[]},
    {"id":"summary","type":"agent","prompt":"Собери итог","after":["leaf-a","leaf-b"]}
  ]
}`), Task: "Проверить terminal skipped-замыкание", CWD: t.TempDir()}
}

// activeSkippedTargetInput моделирует общий target с двумя независимыми
// причинами. Первый shared уже исполняется как start, а невыбранная ветка
// решения должна сохранить второй, синтетический instance того же stepId.
func activeSkippedTargetInput(t *testing.T) Input {
	t.Helper()
	return Input{WorkflowJSON: []byte(`{
  "version":2,
  "id":"active-skipped-target",
  "start":["choice","shared"],
  "steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "selected":{"to":["left"]},"other":{"to":["shared"]}}},
    {"id":"shared","type":"agent","prompt":"Уже выполняется","after":[]},
    {"id":"left","type":"agent","prompt":"Выбранная ветка","after":[]},
    {"id":"tail","type":"agent","prompt":"Продолжи shared","after":["shared"]}
  ]
}`), Task: "Проверить active и skipped общего target", CWD: t.TempDir()}
}

// terminalSharedFIFOInput сочетает global finish с двумя instances одного
// logical step. Более ранний start shared может быть Pending/Running, а
// невыбранный route создаёт поздний causal Skipped. Terminal closure обязан
// провести именно причинный instance через tail.after, не запуская tail.
func terminalSharedFIFOInput(t *testing.T) Input {
	t.Helper()
	return Input{WorkflowJSON: []byte(`{
  "version":2,
  "id":"terminal-shared-fifo",
  "start":["choice","shared"],
  "steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "branch":{"to":["shared"]},"safe":{"finish":"succeeded"}}},
    {"id":"shared","type":"agent","prompt":"Общий шаг","after":[]},
    {"id":"tail","type":"agent","prompt":"Хвост","after":["shared"]}
  ]
}`), Task: "Проверить terminal FIFO для skipped", CWD: t.TempDir()}
}

// skippedQuotaInput создаёт один пропущенный и два выбранных instances общего
// target. maxVisits=1 должен разрешить первый реальный запуск независимо от его
// порядкового visit=2, а следующую попытку завершить по квоте с причиной в нём.
func skippedQuotaInput(t *testing.T) Input {
	t.Helper()
	return Input{WorkflowJSON: []byte(`{
  "version":2,
  "id":"skipped-quota",
  "start":["first","second","third"],
  "steps":[
    {"id":"first","type":"agent","prompt":"Первая развилка","after":[],"decisions":{
      "other":{"to":["other"]},"shared":{"to":["shared"]}}},
    {"id":"second","type":"agent","prompt":"Первый запуск","after":[],"decisions":{"go":{"to":["shared"]}}},
    {"id":"third","type":"agent","prompt":"Лишний запуск","after":[],"decisions":{"go":{"to":["shared"]}}},
    {"id":"other","type":"agent","prompt":"Другая ветка","after":[]},
    {"id":"shared","type":"agent","prompt":"Общая ветка","after":[],"maxVisits":1}
  ]
}`), Task: "Проверить квоту после skipped", CWD: t.TempDir()}
}

// boundedLoopInput оставляет параллельный Pending visit, чтобы остановка по
// квоте доказывала семантику всего run, а не только естественную quiescence.
func boundedLoopInput(t *testing.T, successfulLimit bool) Input {
	t.Helper()
	limit := `"maxVisits":2`
	if successfulLimit {
		limit += `,"onLimit":"succeeded"`
	}
	workflowJSON := strings.Replace(`{
  "version":2,"id":"bounded-loop","start":["loop","parallel"],"steps":[
    {"id":"loop","type":"agent","prompt":"Повтори","after":[],"maxVisits":2,"decisions":{
      "repeat":{"to":["loop"]},"done":{"finish":"succeeded"}}},
    {"id":"parallel","type":"agent","prompt":"Параллельно","after":[]}
  ]
}`, `"maxVisits":2`, limit, 1)
	return Input{WorkflowJSON: []byte(workflowJSON), Task: "Проверить ограниченный цикл", CWD: t.TempDir()}
}

func testAdvanceRun(t *testing.T) (string, Snapshot, *LockedRun) {
	t.Helper()
	root := t.TempDir()
	snapshot, err := Create(root, advanceInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := run.Close(); err != nil {
			t.Error(err)
		}
	})
	return root, snapshot, run
}

func finishDecisionVisit(t *testing.T, run *LockedRun, visitID, key string) {
	t.Helper()
	chatID, turnID, callID := "chat-"+visitID, "turn-"+visitID, "call-"+visitID
	if err := run.ReserveVisits([]string{visitID}); err == nil {
		err = run.UpdateVisit(visitID, scheduler.Unknown, chatID, "")
		if err == nil {
			err = run.SetVisitTurn(visitID, turnID)
		}
		if err == nil {
			_, err = run.CommitDecision(visitID, chatID, turnID, key, "выбран маршрут", callID)
		}
		if err == nil {
			err = run.UpdateVisit(visitID, scheduler.Succeeded, chatID, "")
		}
		if err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}
}

// finishPlainVisit проводит обычное посещение через те же durable-фазы, что и
// coordinator. Тесты переходов не подменяют metadata напрямую, поэтому каждый
// промежуточный снимок проходит production-валидацию.
func finishPlainVisit(t *testing.T, run *LockedRun, visitID string) {
	t.Helper()
	chatID, turnID := "chat-"+visitID, "turn-"+visitID
	if err := run.ReserveVisits([]string{visitID}); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(visitID, scheduler.Unknown, chatID, ""); err != nil {
		t.Fatal(err)
	}
	if err := run.SetVisitTurn(visitID, turnID); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(visitID, scheduler.Succeeded, chatID, ""); err != nil {
		t.Fatal(err)
	}
}

// runDecisionVisit сохраняет выбор, но оставляет turn активным. Это позволяет
// воспроизвести окно, в котором tool уже commit-нул route, а финальный Result
// Codex ещё не сделал decision visit технически завершённым.
func runDecisionVisit(t *testing.T, run *LockedRun, visitID, key string) string {
	t.Helper()
	chatID, turnID := "chat-"+visitID, "turn-"+visitID
	if err := run.ReserveVisits([]string{visitID}); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(visitID, scheduler.Unknown, chatID, ""); err != nil {
		t.Fatal(err)
	}
	if err := run.SetVisitTurn(visitID, turnID); err != nil {
		t.Fatal(err)
	}
	if _, err := run.CommitDecision(visitID, chatID, turnID, key, "выбран маршрут", "call-"+visitID); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(visitID, scheduler.Running, chatID, ""); err != nil {
		t.Fatal(err)
	}
	return chatID
}

// runDecisionlessVisit оставляет обычный turn Running для проверки terminal
// drain: после остановки такой уже начатый внешний запрос ещё может сообщить
// финальное техническое состояние, но не вправе изменить исход всего run.
func runDecisionlessVisit(t *testing.T, run *LockedRun, visitID string) string {
	t.Helper()
	chatID, turnID := "chat-"+visitID, "turn-"+visitID
	if err := run.ReserveVisits([]string{visitID}); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(visitID, scheduler.Unknown, chatID, ""); err != nil {
		t.Fatal(err)
	}
	if err := run.SetVisitTurn(visitID, turnID); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(visitID, scheduler.Running, chatID, ""); err != nil {
		t.Fatal(err)
	}
	return chatID
}

// testBoundedLoopAtLimit создаёт оба разрешённых посещения и сохраняет решение
// repeat у второго, но намеренно не вызывает последний Advance. Возвращённый
// snapshot остаётся Running, а следующий переход обязан быть limit-terminal.
func testBoundedLoopAtLimit(t *testing.T, successfulLimit bool) (string, Snapshot, *LockedRun, Visit) {
	t.Helper()
	root := t.TempDir()
	initial, err := Create(root, boundedLoopInput(t, successfulLimit))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := run.Close(); err != nil {
			t.Error(err)
		}
	})
	finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "repeat")
	first, err := run.AdvanceAgentGraph()
	if err != nil || len(first.CreatedVisits) != 1 || first.CreatedVisits[0].StepID != "loop" || first.CreatedVisits[0].Visit != 2 || first.CreatedVisits[0].Iteration != 2 {
		t.Fatalf("второе посещение цикла не создано: %+v, %v", first, err)
	}
	second := first.CreatedVisits[0]
	finishDecisionVisit(t, run, second.VisitID, "repeat")
	return root, initial, run, second
}

// TestAdvanceAgentGraphRouteAndAfter проверяет два последовательных durable
// перехода: decision fanout и after после технического Failed одной из веток.
// Повтор каждого Advance не создаёт новый visit или файл памяти.
func TestAdvanceAgentGraphRouteAndAfter(t *testing.T) {
	root, initial, run := testAdvanceRun(t)
	decisionID := initial.Meta.Visits[0].VisitID
	finishDecisionVisit(t, run, decisionID, "branch")

	advanced, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if !advanced.Changed || len(advanced.Plan.ApplyDecisionVisitIDs) != 1 || len(advanced.CreatedVisits) != 2 {
		t.Fatalf("fanout применён не полностью: %+v", advanced)
	}
	for index, stepID := range []string{"left", "right"} {
		visit := advanced.CreatedVisits[index]
		if visit.StepID != stepID || visit.Visit != 1 || visit.Iteration != 2 || visit.State != scheduler.Pending ||
			visit.Trigger.Kind != TriggerDecision || !reflect.DeepEqual(visit.Trigger.SourceVisitIDs, []string{decisionID}) || visit.Trigger.DecisionKey != "branch" {
			t.Fatalf("неверное decision-посещение: %+v", visit)
		}
		if info, err := os.Stat(filepath.Join(root, initial.Meta.RunID, "memory", visit.VisitID+".md")); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("нет памяти нового visit: %v, %v", info, err)
		}
	}
	if !advanced.Snapshot.Meta.Visits[0].Decision.Applied {
		t.Fatal("decision и targets не опубликованы одним commit")
	}
	beforeRetry := advanced.Snapshot.Meta
	retry, err := run.AdvanceAgentGraph()
	if err != nil || retry.Changed || !reflect.DeepEqual(retry.Snapshot.Meta, beforeRetry) {
		t.Fatalf("повтор fanout создал дубликаты: %+v, %v", retry, err)
	}

	left, right := advanced.CreatedVisits[0], advanced.CreatedVisits[1]
	if err := run.ReserveVisits([]string{left.VisitID, right.VisitID}); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(left.VisitID, scheduler.Unknown, "chat-left", ""); err == nil {
		err = run.UpdateVisit(left.VisitID, scheduler.Failed, "chat-left", "тест упал")
		if err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(right.VisitID, scheduler.Unknown, "chat-right", ""); err == nil {
		err = run.SetVisitTurn(right.VisitID, "turn-right")
		if err == nil {
			err = run.UpdateVisit(right.VisitID, scheduler.Succeeded, "chat-right", "")
		}
		if err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}

	joined, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if !joined.Changed || len(joined.Plan.AfterActivations) != 1 || len(joined.CreatedVisits) != 1 {
		t.Fatalf("after-barrier не материализован: %+v", joined)
	}
	join := joined.CreatedVisits[0]
	if join.StepID != "join" || join.Iteration != 2 || join.Trigger.Kind != TriggerAfter ||
		!reflect.DeepEqual(join.Trigger.SourceVisitIDs, []string{left.VisitID, right.VisitID}) {
		t.Fatalf("after не сохранил ordered Failed/Succeeded sources: %+v", join)
	}
	if retry, err = run.AdvanceAgentGraph(); err != nil || retry.Changed || len(retry.Snapshot.Meta.Visits) != len(joined.Snapshot.Meta.Visits) {
		t.Fatalf("повтор after создал дубликат: %+v, %v", retry, err)
	}
}

// TestAdvanceAgentGraphPersistsSkippedBranchAndUnblocksJoin проверяет всю
// persistence-цепочку if/else. Skipped получает собственные ID, trigger и пустую
// память, но не запускается; смешанный Succeeded/Skipped barrier создаёт обычный
// Pending join с точными причинными visitId.
func TestAdvanceAgentGraphPersistsSkippedBranchAndUnblocksJoin(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, skippedJoinInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	choiceID := initial.Meta.Visits[0].VisitID
	finishDecisionVisit(t, run, choiceID, "left")
	advanced, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if !advanced.Changed || len(advanced.CreatedVisits) != 2 || !advanced.Snapshot.Meta.Visits[0].Decision.Applied {
		t.Fatalf("выбор и обе branch-записи не опубликованы одним переходом: %+v", advanced)
	}
	left, right := advanced.CreatedVisits[0], advanced.CreatedVisits[1]
	if left.StepID != "left" || left.State != scheduler.Pending || left.Trigger.Kind != TriggerDecision ||
		right.StepID != "right" || right.State != scheduler.Skipped || right.Trigger.Kind != TriggerDecisionSkipped ||
		right.Trigger.DecisionKey != "right" || !slices.Equal(right.Trigger.SourceVisitIDs, []string{choiceID}) {
		t.Fatalf("ветки получили неверные состояния или причины: left=%+v right=%+v", left, right)
	}
	if info, statErr := os.Stat(filepath.Join(root, initial.Meta.RunID, "memory", right.VisitID+".md")); statErr != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatalf("Skipped visit не получил пустую durable-память: info=%v err=%v", info, statErr)
	}
	if err = run.ReserveVisits([]string{right.VisitID}); err == nil {
		t.Fatal("Skipped visit принят для запуска Codex")
	}

	finishPlainVisit(t, run, left.VisitID)
	joined, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if len(joined.CreatedVisits) != 1 {
		t.Fatalf("смешанный barrier не создал ровно один join: %+v", joined)
	}
	join := joined.CreatedVisits[0]
	if join.StepID != "join" || join.State != scheduler.Pending || join.Trigger.Kind != TriggerAfter ||
		!slices.Equal(join.Trigger.SourceVisitIDs, []string{left.VisitID, right.VisitID}) {
		t.Fatalf("join потерял выполненную или пропущенную причину: %+v", join)
	}
}

// TestAdvanceAgentGraphPropagatesNestedSkippedBranch проверяет пользовательский
// сценарий из ревью целиком. После выбора right внутренний decision и оба его
// возможных листа становятся Skipped без запуска; реальная правая ветка затем
// вместе с этими причинными tokens открывает обычный смешанный join.
func TestAdvanceAgentGraphPropagatesNestedSkippedBranch(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, nestedSkippedInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	outerID := initial.Meta.Visits[0].VisitID
	finishDecisionVisit(t, run, outerID, "right")
	first, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	created := make(map[string]Visit, len(first.CreatedVisits))
	for _, visit := range first.CreatedVisits {
		created[visit.StepID] = visit
	}
	if len(first.CreatedVisits) != 4 || created["right"].State != scheduler.Pending ||
		created["inner"].State != scheduler.Skipped || created["leaf-a"].State != scheduler.Skipped ||
		created["leaf-b"].State != scheduler.Skipped {
		t.Fatalf("вложенная skipped-ветка материализована не полностью: %+v", first.CreatedVisits)
	}
	inner := created["inner"]
	if inner.Trigger.Kind != TriggerDecisionSkipped || !slices.Equal(inner.Trigger.SourceVisitIDs, []string{outerID}) ||
		created["leaf-a"].Trigger.Kind != TriggerDecisionSkipped ||
		!slices.Equal(created["leaf-a"].Trigger.SourceVisitIDs, []string{inner.VisitID}) ||
		!slices.Equal(created["leaf-b"].Trigger.SourceVisitIDs, []string{inner.VisitID}) {
		t.Fatalf("вложенная ветка потеряла immediate causal links: %+v", first.CreatedVisits)
	}

	finishPlainVisit(t, run, created["right"].VisitID)
	joined, err := run.AdvanceAgentGraph()
	if err != nil || len(joined.CreatedVisits) != 1 {
		t.Fatalf("смешанный join не открылся: %+v, %v", joined, err)
	}
	join := joined.CreatedVisits[0]
	if join.StepID != "join" || join.State != scheduler.Pending || join.Trigger.Kind != TriggerAfter ||
		!slices.Equal(join.Trigger.SourceVisitIDs, []string{
			created["right"].VisitID, created["leaf-a"].VisitID, created["leaf-b"].VisitID,
		}) {
		t.Fatalf("join не сохранил real/skipped sources: %+v", join)
	}
	finishPlainVisit(t, run, join.VisitID)
	finished, err := run.AdvanceAgentGraph()
	if err != nil || finished.Snapshot.Meta.RunState != RunSucceeded || finished.Plan.Terminal == nil {
		t.Fatalf("граф не достиг естественного успеха после join: %+v, %v", finished, err)
	}
}

// TestAdvanceAgentGraphClosesSkippedBranchBeforeFinish доказывает атомарность
// terminal boundary. Даже если choice немедленно выбрал finish, runstore сначала
// выдаёт настоящие visitId всей вложенной skipped-цепочке и лишь потом публикует
// RunSucceeded; отдельного post-terminal Advance для ремонта не требуется.
func TestAdvanceAgentGraphClosesSkippedBranchBeforeFinish(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, terminalNestedSkippedInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	choiceID := initial.Meta.Visits[0].VisitID
	finishDecisionVisit(t, run, choiceID, "safe")
	result, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Meta.RunState != RunSucceeded || result.Snapshot.Meta.StopVisitID != choiceID ||
		result.Plan.Terminal == nil || len(result.CreatedVisits) != 4 {
		t.Fatalf("finish опубликован до полного skipped-замыкания: %+v", result)
	}
	created := make(map[string]Visit, len(result.CreatedVisits))
	for _, visit := range result.CreatedVisits {
		created[visit.StepID] = visit
		if visit.State != scheduler.Skipped || visit.CodexThreadID != "" || visit.TurnID != "" || visit.Attempt != 0 {
			t.Fatalf("terminal closure попытался выполнить пропущенный visit: %+v", visit)
		}
		info, statErr := os.Stat(filepath.Join(root, initial.Meta.RunID, "memory", visit.VisitID+".md"))
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			t.Fatalf("пропущенный visit не получил пустую durable-память: %v, %v", info, statErr)
		}
	}
	inner, leafA, leafB, summary := created["inner"], created["leaf-a"], created["leaf-b"], created["summary"]
	if inner.VisitID == "" || leafA.VisitID == "" || leafB.VisitID == "" || summary.VisitID == "" ||
		!slices.Equal(inner.Trigger.SourceVisitIDs, []string{choiceID}) ||
		!slices.Equal(leafA.Trigger.SourceVisitIDs, []string{inner.VisitID}) ||
		!slices.Equal(leafB.Trigger.SourceVisitIDs, []string{inner.VisitID}) ||
		summary.Trigger.Kind != TriggerAfter || !slices.Equal(summary.Trigger.SourceVisitIDs, []string{leafA.VisitID, leafB.VisitID}) {
		t.Fatalf("terminal closure потерял причинную цепочку: %+v", result.CreatedVisits)
	}
	if reloaded, loadErr := Load(root, initial.Meta.RunID); loadErr != nil || !reflect.DeepEqual(reloaded.Meta, result.Snapshot.Meta) {
		t.Fatalf("полное terminal-замыкание не прошло повторную валидацию: %+v, %v", reloaded.Meta, loadErr)
	}
}

// TestAdvanceAgentGraphTerminalClosureCrossesSharedFIFO проверяет два способа,
// которыми ранний instance общего step раньше блокировал causal Skipped навсегда.
// Pending превращается в rootless cleanup и должен оставить отдельную
// after-квитанцию; Running нельзя подменить, поэтому terminal closure устойчиво
// обходит его и связывает tail с поздним causal source. В обоих случаях весь
// результат публикуется тем же commit, что и finish.
func TestAdvanceAgentGraphTerminalClosureCrossesSharedFIFO(t *testing.T) {
	for _, variant := range []struct {
		name        string
		runShared   bool
		wantTailNum int
	}{
		{name: "pending cleanup receipt", wantTailNum: 2},
		{name: "running source is abandoned", runShared: true, wantTailNum: 1},
	} {
		t.Run(variant.name, func(t *testing.T) {
			root := t.TempDir()
			initial, err := Create(root, terminalSharedFIFOInput(t))
			if err != nil {
				t.Fatal(err)
			}
			run, err := OpenLocked(root, initial.Meta.RunID)
			if err != nil {
				t.Fatal(err)
			}
			defer run.Close()

			choiceID, sharedID := initial.Meta.Visits[0].VisitID, initial.Meta.Visits[1].VisitID
			sharedChat := ""
			if variant.runShared {
				sharedChat = runDecisionlessVisit(t, run, sharedID)
			}
			finishDecisionVisit(t, run, choiceID, "safe")
			result, err := run.AdvanceAgentGraph()
			if err != nil || result.Snapshot.Meta.RunState != RunSucceeded {
				t.Fatalf("terminal closure не опубликован: %+v, %v", result, err)
			}

			var causalShared Visit
			var tails []Visit
			for _, visit := range result.CreatedVisits {
				switch visit.StepID {
				case "shared":
					causalShared = visit
				case "tail":
					tails = append(tails, visit)
				}
				if visit.State != scheduler.Skipped {
					t.Fatalf("terminal closure попытался запустить visit: %+v", visit)
				}
			}
			if causalShared.Trigger.Kind != TriggerDecisionSkipped ||
				!slices.Equal(causalShared.Trigger.SourceVisitIDs, []string{choiceID}) || len(tails) != variant.wantTailNum {
				t.Fatalf("causal shared или tail-получатели потеряны: %+v", result.CreatedVisits)
			}
			causalTail := tails[len(tails)-1]
			if causalTail.Trigger.Kind != TriggerAfter ||
				!slices.Equal(causalTail.Trigger.SourceVisitIDs, []string{causalShared.VisitID}) {
				t.Fatalf("последний tail не продолжил causal source: %+v", tails)
			}
			if !variant.runShared && !slices.Equal(tails[0].Trigger.SourceVisitIDs, []string{sharedID}) {
				t.Fatalf("cleanup source не оставил первую FIFO-квитанцию: %+v", tails)
			}
			if variant.runShared {
				if err = run.UpdateVisit(sharedID, scheduler.Cancelled, sharedChat, "run уже завершён"); err != nil {
					t.Fatalf("terminal drain активного source сломал bypass-инвариант: %v", err)
				}
				if _, err = run.Load(); err != nil {
					t.Fatalf("terminal snapshot не читается после drain: %v", err)
				}
			}
		})
	}
}

// TestAdvanceAgentGraphPersistsSkippedBesideActiveTarget проверяет liveness
// общей ноды. Синтетический Skipped не запускает executor и потому не обязан
// ждать завершения уже активного visit того же шага; весь decision transition
// сохраняется сразу и остаётся валидным после повторного чтения metadata.
func TestAdvanceAgentGraphPersistsSkippedBesideActiveTarget(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, activeSkippedTargetInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	choiceID, sharedID := initial.Meta.Visits[0].VisitID, initial.Meta.Visits[1].VisitID
	sharedChat := runDecisionlessVisit(t, run, sharedID)
	finishDecisionVisit(t, run, choiceID, "selected")
	advanced, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if !advanced.Changed || !advanced.Snapshot.Meta.Visits[0].Decision.Applied || len(advanced.CreatedVisits) != 2 {
		t.Fatalf("активный общий target заблокировал decision transition: %+v", advanced)
	}
	left, skippedShared := advanced.CreatedVisits[0], advanced.CreatedVisits[1]
	if left.StepID != "left" || left.State != scheduler.Pending ||
		skippedShared.StepID != "shared" || skippedShared.Visit != 2 || skippedShared.State != scheduler.Skipped ||
		skippedShared.Trigger.Kind != TriggerDecisionSkipped || !slices.Equal(skippedShared.Trigger.SourceVisitIDs, []string{choiceID}) {
		t.Fatalf("selected и skipped instances сохранены неверно: left=%+v shared=%+v", left, skippedShared)
	}
	reloaded, err := run.Load()
	if err != nil || reloaded.Meta.Visits[1].State != scheduler.Running || reloaded.Meta.Visits[3].State != scheduler.Skipped {
		t.Fatalf("Running и Skipped одного stepId не пережили валидацию: %+v, %v", reloaded.Meta.Visits, err)
	}

	// Завершение более раннего real visit не должно ретроактивно сломать FIFO.
	// Первый tail потребляет его, затем skip-only closure тем же commit забирает
	// следующий Skipped-source. Оба instances упорядочены, а второй не ждёт
	// исполнения первого, потому что сам не занимает executor.
	if err = run.UpdateVisit(sharedID, scheduler.Succeeded, sharedChat, ""); err != nil {
		t.Fatalf("завершение real visit после Skipped повредило историю: %v", err)
	}
	firstTail, err := run.AdvanceAgentGraph()
	if err != nil || len(firstTail.CreatedVisits) != 2 || firstTail.CreatedVisits[0].StepID != "tail" ||
		firstTail.CreatedVisits[0].State != scheduler.Pending || !slices.Equal(firstTail.CreatedVisits[0].Trigger.SourceVisitIDs, []string{sharedID}) ||
		firstTail.CreatedVisits[1].StepID != "tail" || firstTail.CreatedVisits[1].State != scheduler.Skipped ||
		!slices.Equal(firstTail.CreatedVisits[1].Trigger.SourceVisitIDs, []string{skippedShared.VisitID}) {
		t.Fatalf("FIFO не отдал ранний real visit первым: %+v, %v", firstTail, err)
	}
}

// TestAdvanceAgentGraphTerminalMarksPendingAndBranchesSkipped фиксирует один
// атомарный terminal commit: ещё не запущенный start и цели невыбранного route
// становятся Skipped, а внешний callback не может изготовить такой статус сам.
func TestAdvanceAgentGraphTerminalMarksPendingAndBranchesSkipped(t *testing.T) {
	root, initial, run := testAdvanceRun(t)
	choiceID, parallelID := initial.Meta.Visits[0].VisitID, initial.Meta.Visits[1].VisitID
	finishDecisionVisit(t, run, choiceID, "done")

	result, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Meta.RunState != RunSucceeded || result.Snapshot.Meta.Visits[1].State != scheduler.Skipped ||
		len(result.CreatedVisits) != 3 {
		t.Fatalf("terminal не закрыл Pending и невыбранный fanout: %+v", result)
	}
	for _, visit := range result.Snapshot.Meta.Visits {
		if visit.State == scheduler.Pending {
			t.Fatalf("terminal snapshot оставил Pending visit: %+v", visit)
		}
	}
	for _, visit := range result.CreatedVisits {
		if visit.State != scheduler.Skipped {
			t.Fatalf("невыбранная ветка не сохранена как synthetic Skipped: %+v", visit)
		}
		if visit.StepID == "join" {
			if visit.Trigger.Kind != TriggerAfter || len(visit.Trigger.SourceVisitIDs) != 2 {
				t.Fatalf("terminal closure не распространил skipped через join: %+v", visit)
			}
		} else if visit.Trigger.Kind != TriggerDecisionSkipped || !slices.Equal(visit.Trigger.SourceVisitIDs, []string{choiceID}) {
			t.Fatalf("невыбранный route потерял решение-источник: %+v", visit)
		}
		if info, statErr := os.Stat(filepath.Join(root, initial.Meta.RunID, "memory", visit.VisitID+".md")); statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("нет памяти synthetic Skipped %q: %v", visit.VisitID, statErr)
		}
	}
	if err = run.UpdateVisit(parallelID, scheduler.Skipped, "", ""); err == nil {
		t.Fatal("UpdateVisit позволил внешнему коду изготовить Skipped")
	}
}

// TestAdvanceAgentGraphSkippedDoesNotConsumeMaxVisits связывает два счётчика:
// Visit остаётся монотонным номером всей истории, а maxVisits учитывает только
// записи, которые действительно могли создать Codex turn.
func TestAdvanceAgentGraphSkippedDoesNotConsumeMaxVisits(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, skippedQuotaInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "other")
	finishDecisionVisit(t, run, initial.Meta.Visits[1].VisitID, "go")
	finishDecisionVisit(t, run, initial.Meta.Visits[2].VisitID, "go")
	first, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	var skipped, runnable Visit
	for _, visit := range first.CreatedVisits {
		if visit.StepID != "shared" {
			continue
		}
		if visit.State == scheduler.Skipped {
			skipped = visit
		} else {
			runnable = visit
		}
	}
	if skipped.Visit != 1 || runnable.Visit != 2 || runnable.State != scheduler.Pending {
		t.Fatalf("Skipped съел квоту или нарушил нумерацию: skipped=%+v runnable=%+v", skipped, runnable)
	}
	finishPlainVisit(t, run, runnable.VisitID)

	limited, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if limited.Snapshot.Meta.RunState != RunFailed || limited.Snapshot.Meta.StopLimitStepID != "shared" ||
		limited.Snapshot.Meta.StopVisitID != runnable.VisitID || limited.Snapshot.Meta.StopLimitIteration != 2 ||
		!strings.Contains(limited.Snapshot.Meta.StopReason, "исполнение №2") {
		t.Fatalf("limit не сослался на единственный runnable visit: %+v", limited)
	}
}

// TestAdvanceAgentGraphFinishWithActiveVisit фиксирует приоритет explicit finish:
// он атомарно связывается с Applied decision и останавливает планирование, не
// переписывая состояние уже работающего параллельного visit.
func TestAdvanceAgentGraphFinishWithActiveVisit(t *testing.T) {
	_, initial, run := testAdvanceRun(t)
	decisionID, parallelID := initial.Meta.Visits[0].VisitID, initial.Meta.Visits[1].VisitID
	finishDecisionVisit(t, run, decisionID, "done")
	if err := run.ReserveVisits([]string{parallelID}); err == nil {
		err = run.UpdateVisit(parallelID, scheduler.Unknown, "chat-parallel", "")
		if err == nil {
			err = run.SetVisitTurn(parallelID, "turn-parallel")
		}
		if err == nil {
			err = run.UpdateVisit(parallelID, scheduler.Running, "chat-parallel", "")
		}
		if err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}

	result, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Plan.Terminal == nil || result.Snapshot.Meta.RunState != RunSucceeded ||
		result.Snapshot.Meta.StopReason == "" || result.Snapshot.Meta.StopVisitID != decisionID ||
		!result.Snapshot.Meta.Visits[0].Decision.Applied || result.Snapshot.Meta.Visits[1].State != scheduler.Running || len(result.CreatedVisits) != 3 {
		t.Fatalf("finish опубликован неатомарно: %+v", result)
	}
	for _, visit := range result.CreatedVisits {
		if visit.State != scheduler.Skipped ||
			visit.StepID == "join" && visit.Trigger.Kind != TriggerAfter ||
			visit.StepID != "join" && visit.Trigger.Kind != TriggerDecisionSkipped {
			t.Fatalf("finish не сохранил невыбранную ветку: %+v", visit)
		}
	}
	if _, err := run.AdvanceAgentGraph(); err == nil {
		t.Fatal("terminal run принят для повторного планирования")
	}
}

// TestAdvanceAgentGraphFatalDecision сохраняет проверяемый CauseVisitID, когда
// успешный decision-turn завершился без обязательного choose_decision.
func TestAdvanceAgentGraphFatalDecision(t *testing.T) {
	_, initial, run := testAdvanceRun(t)
	visitID := initial.Meta.Visits[0].VisitID
	if err := run.ReserveVisits([]string{visitID}); err == nil {
		err = run.UpdateVisit(visitID, scheduler.Unknown, "chat-decision", "")
		if err == nil {
			err = run.SetVisitTurn(visitID, "turn-decision")
		}
		if err == nil {
			err = run.UpdateVisit(visitID, scheduler.Succeeded, "chat-decision", "")
		}
		if err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}
	result, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Meta.RunState != RunFailed || result.Snapshot.Meta.StopVisitID != visitID || result.Snapshot.Meta.StopReason == "" || len(result.CreatedVisits) != 0 {
		t.Fatalf("fatal decision не сохранил доказанную причину: %+v", result)
	}
}

// TestAdvanceAgentGraphCompactsStopReason воспроизводит корректный workflow с
// очень длинным decision key и управляющим символом. Choice остаётся точным в
// durable DecisionRecord, но производная операторская причина обязана пройти
// ограничения metadata и не блокировать применение terminal route.
func TestAdvanceAgentGraphCompactsStopReason(t *testing.T) {
	input := advanceInput(t)
	key := strings.Repeat("решение🙂", 600) + "\x1b"
	encodedKey, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	input.WorkflowJSON = bytes.Replace(input.WorkflowJSON, []byte(`"fail"`), encodedKey, 1)
	root := t.TempDir()
	initial, err := Create(root, input)
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, key)

	result, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.Meta.RunState != RunFailed || !result.Snapshot.Meta.Visits[0].Decision.Applied ||
		!safeStoredText(result.Snapshot.Meta.StopReason, true) || !strings.HasSuffix(result.Snapshot.Meta.StopReason, "…") ||
		strings.ContainsRune(result.Snapshot.Meta.StopReason, '\x1b') {
		t.Fatalf("terminal reason не нормализована безопасно: state=%q reason=%q", result.Snapshot.Meta.RunState, result.Snapshot.Meta.StopReason)
	}
}

// TestAdvanceAgentGraphMemoryBeforeMetadata воспроизводит отказ Sync meta до
// Rename. Новая память уже durable, но остаётся orphan; старая metadata цела,
// владелец poisoned, а reopen строит новые IDs и применяет маршрут ровно один раз.
func TestAdvanceAgentGraphMemoryBeforeMetadata(t *testing.T) {
	root, initial, run := testAdvanceRun(t)
	finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "branch")
	memoryDir := filepath.Join(root, initial.Meta.RunID, "memory")
	memoriesReady := false
	result, err := run.advanceAgentGraph(func(file *os.File) error {
		info, statErr := file.Stat()
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() && info.Size() > 0 {
			entries, readErr := os.ReadDir(memoryDir)
			memoriesReady = readErr == nil && len(entries) == 4
			return syscall.EIO
		}
		return file.Sync()
	})
	if !errors.Is(err, syscall.EIO) || !memoriesReady || !reflect.DeepEqual(result, AgentAdvance{}) {
		t.Fatalf("не воспроизведена граница memory-before-meta: %+v, %v", result, err)
	}
	if _, err := run.Load(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("владелец продолжил после неоднозначной записи meta: %v", err)
	}
	onDisk, err := Load(root, initial.Meta.RunID)
	if err != nil || len(onDisk.Meta.Visits) != 2 || onDisk.Meta.Visits[0].Decision.Applied {
		t.Fatalf("отказ до Rename частично опубликовал plan: %+v, %v", onDisk.Meta, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if result, err = reopened.AdvanceAgentGraph(); err != nil || !result.Changed || len(result.CreatedVisits) != 2 {
		t.Fatalf("reopen не применил сохранённое решение: %+v, %v", result, err)
	}
	entries, err := os.ReadDir(memoryDir)
	if err != nil || len(entries) != 6 {
		t.Fatalf("orphan memory потеряна или стала достижимой: %v, %v", entries, err)
	}
}

// TestAdvanceAgentGraphSkippedSurvivesCrashAndReopen проверяет новую часть той
// же durability-границы: если запись meta оборвалась после пустых файлов памяти,
// reopen повторно строит ровно один synthetic Skipped с новым ID, а orphan-файл
// не становится частью причинной истории.
func TestAdvanceAgentGraphSkippedSurvivesCrashAndReopen(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, skippedJoinInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "left")
	memoryDir := filepath.Join(root, initial.Meta.RunID, "memory")
	memoriesReady := false
	result, err := run.advanceAgentGraph(func(file *os.File) error {
		info, statErr := file.Stat()
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() && info.Size() > 0 {
			entries, readErr := os.ReadDir(memoryDir)
			memoriesReady = readErr == nil && len(entries) == 3
			return syscall.EIO
		}
		return file.Sync()
	})
	if !errors.Is(err, syscall.EIO) || !memoriesReady || !reflect.DeepEqual(result, AgentAdvance{}) {
		t.Fatalf("не воспроизведена crash-граница skipped: %+v, %v", result, err)
	}
	onDisk, err := Load(root, initial.Meta.RunID)
	if err != nil || len(onDisk.Meta.Visits) != 1 || onDisk.Meta.Visits[0].Decision.Applied {
		t.Fatalf("отказ частично опубликовал skipped-переход: %+v, %v", onDisk.Meta, err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err = reopened.AdvanceAgentGraph()
	if err != nil || len(result.CreatedVisits) != 2 || result.CreatedVisits[1].State != scheduler.Skipped {
		t.Fatalf("reopen не восстановил selected/skipped пару: %+v, %v", result, err)
	}
	entries, err := os.ReadDir(memoryDir)
	if err != nil || len(entries) != 5 {
		t.Fatalf("orphan skipped-memory потеряна или стала достижимой: %v, %v", entries, err)
	}
}

// TestAdvanceAgentGraphTerminalSkippedClosureSurvivesCrash проверяет новую
// многоуровневую часть той же durability-границы. Все memory-файлы closure
// создаются до одного Rename meta; сбой оставляет только недостижимые orphan,
// а reopen заново строит полную цепочку с новыми ID без частичного terminal.
func TestAdvanceAgentGraphTerminalSkippedClosureSurvivesCrash(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, terminalNestedSkippedInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "safe")
	memoryDir := filepath.Join(root, initial.Meta.RunID, "memory")
	memoriesReady := false
	result, err := run.advanceAgentGraph(func(file *os.File) error {
		info, statErr := file.Stat()
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() && info.Size() > 0 {
			entries, readErr := os.ReadDir(memoryDir)
			memoriesReady = readErr == nil && len(entries) == 5
			return syscall.EIO
		}
		return file.Sync()
	})
	if !errors.Is(err, syscall.EIO) || !memoriesReady || !reflect.DeepEqual(result, AgentAdvance{}) {
		t.Fatalf("не воспроизведён crash после skipped-замыкания: %+v, %v", result, err)
	}
	onDisk, err := Load(root, initial.Meta.RunID)
	if err != nil || onDisk.Meta.RunState != RunRunning || onDisk.Meta.Visits[0].Decision.Applied || len(onDisk.Meta.Visits) != 1 {
		t.Fatalf("сбой частично опубликовал terminal closure: %+v, %v", onDisk.Meta, err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err = reopened.AdvanceAgentGraph()
	if err != nil || result.Snapshot.Meta.RunState != RunSucceeded || len(result.CreatedVisits) != 4 || len(result.Snapshot.Meta.Visits) != 5 {
		t.Fatalf("reopen не пересобрал terminal closure целиком: %+v, %v", result, err)
	}
	entries, err := os.ReadDir(memoryDir)
	if err != nil || len(entries) != 9 {
		t.Fatalf("orphan closure-memory потеряна или попала в metadata: %v, %v", entries, err)
	}
}

// TestAdvanceAgentGraphBackfillsOldAppliedDecision имитирует running v4,
// сохранённый прежней версией: выбранный target уже материализован и Applied,
// но synthetic Skipped ещё отсутствует. Новый planner достраивает только
// безопасный terminal token и не повторяет реальную работу.
func TestAdvanceAgentGraphBackfillsOldAppliedDecision(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, skippedJoinInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	choiceID := initial.Meta.Visits[0].VisitID
	finishDecisionVisit(t, run, choiceID, "left")
	leftID := newID()
	if err = os.WriteFile(filepath.Join(root, initial.Meta.RunID, "memory", leftID+".md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = run.mutateVisits((*os.File).Sync, func(snapshot *Snapshot) (bool, error) {
		snapshot.Meta.Visits[0].Decision.Applied = true
		snapshot.Meta.Visits = append(snapshot.Meta.Visits, Visit{
			VisitID: leftID, StepID: "left", Visit: 1, Iteration: 2, State: scheduler.Pending,
			Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{choiceID}, DecisionKey: "left"},
		})
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CreatedVisits) != 1 || result.CreatedVisits[0].StepID != "right" ||
		result.CreatedVisits[0].State != scheduler.Skipped || len(result.Snapshot.Meta.Visits) != 3 {
		t.Fatalf("старое Applied-решение не получило точный backfill: %+v", result)
	}
	leftCount := 0
	for _, visit := range result.Snapshot.Meta.Visits {
		if visit.StepID == "left" {
			leftCount++
		}
	}
	if leftCount != 1 {
		t.Fatalf("backfill повторил выбранную работу: %+v", result.Snapshot.Meta.Visits)
	}
}

// TestAdvanceAgentGraphBoundedLoop проверяет durable-границу квоты. Ошибка Sync
// до Rename не публикует частичный terminal, а reopen детерминированно строит тот
// же StopVisitID/StopLimitStepID без третьего visit и без Applied у решения.
func TestAdvanceAgentGraphBoundedLoop(t *testing.T) {
	t.Run("default failed survives crash and reopen", func(t *testing.T) {
		root, initial, run, second := testBoundedLoopAtLimit(t, false)
		result, err := run.advanceAgentGraph(func(*os.File) error { return syscall.EIO })
		if !errors.Is(err, syscall.EIO) || !reflect.DeepEqual(result, AgentAdvance{}) {
			t.Fatalf("отказ записи limit-terminal не воспроизведён: %+v, %v", result, err)
		}
		onDisk, err := Load(root, initial.Meta.RunID)
		if err != nil || onDisk.Meta.RunState != RunRunning || onDisk.Meta.StopReason != "" || onDisk.Meta.StopVisitID != "" ||
			onDisk.Meta.StopLimitStepID != "" || onDisk.Meta.StopLimitTrigger != nil || onDisk.Meta.StopLimitIteration != 0 ||
			len(onDisk.Meta.Visits) != 3 || onDisk.Meta.Visits[2].Decision.Applied {
			t.Fatalf("Sync до Rename частично опубликовал limit: %+v, %v", onDisk.Meta, err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenLocked(root, initial.Meta.RunID)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		result, err = reopened.AdvanceAgentGraph()
		if err != nil || !result.Changed || result.Snapshot.Meta.RunState != RunFailed ||
			result.Snapshot.Meta.StopVisitID != second.VisitID || result.Snapshot.Meta.StopLimitStepID != "loop" ||
			result.Snapshot.Meta.StopLimitTrigger == nil || result.Snapshot.Meta.StopLimitTrigger.Kind != TriggerDecision ||
			!slices.Equal(result.Snapshot.Meta.StopLimitTrigger.SourceVisitIDs, []string{second.VisitID}) ||
			result.Snapshot.Meta.StopLimitTrigger.DecisionKey != "repeat" || result.Snapshot.Meta.StopLimitIteration != 3 ||
			result.Plan.Terminal == nil || result.Plan.Terminal.LimitStepID != "loop" ||
			result.Plan.Terminal.LimitTrigger.Kind != scheduler.AgentTriggerDecision ||
			!slices.Equal(result.Plan.Terminal.LimitTrigger.SourceVisitIDs, []string{second.VisitID}) ||
			result.Plan.Terminal.LimitTrigger.DecisionKey != "repeat" || result.Plan.Terminal.LimitIteration != 3 || len(result.CreatedVisits) != 0 ||
			len(result.Snapshot.Meta.Visits) != 3 ||
			result.Snapshot.Meta.Visits[2].Decision.Applied {
			t.Fatalf("reopen не опубликовал точный limit-terminal: %+v, %v", result, err)
		}

		// Каждая подмена ломает отдельную часть структурного proof: тип причины,
		// последний разрешённый visit, onLimit outcome либо сам предел N.
		for name, mutate := range map[string]func(*Snapshot){
			"missing marker":       func(s *Snapshot) { s.Meta.StopLimitStepID = "" },
			"missing trigger":      func(s *Snapshot) { s.Meta.StopLimitTrigger = nil },
			"missing iteration":    func(s *Snapshot) { s.Meta.StopLimitIteration = 0 },
			"step without limit":   func(s *Snapshot) { s.Meta.StopLimitStepID = "parallel" },
			"wrong stop visit":     func(s *Snapshot) { s.Meta.StopVisitID = initial.Meta.Visits[0].VisitID },
			"wrong outcome":        func(s *Snapshot) { s.Meta.RunState = RunSucceeded },
			"wrong trigger source": func(s *Snapshot) { s.Meta.StopLimitTrigger.SourceVisitIDs = []string{initial.Meta.Visits[0].VisitID} },
			"wrong trigger key":    func(s *Snapshot) { s.Meta.StopLimitTrigger.DecisionKey = "done" },
			"wrong iteration":      func(s *Snapshot) { s.Meta.StopLimitIteration = 4 },
			"open decision wave": func(s *Snapshot) {
				s.Workflow.Steps = slices.Clone(s.Workflow.Steps)
				outcome := workflow.OutcomeSucceeded
				s.Workflow.Steps[1].Decisions = map[string]workflow.Route{"done": {Finish: &outcome}}
				s.Meta.Visits[1].State, s.Meta.Visits[1].CodexThreadID = scheduler.Running, "chat-open-wave"
				s.Meta.Visits[1].TurnID, s.Meta.Visits[1].Attempt = "turn-open-wave", 1
			},
			"no pending activation": func(s *Snapshot) {
				s.Meta.Visits[2].State, s.Meta.Visits[2].Decision = scheduler.Failed, nil
				s.Meta.Visits[2].TechnicalError = "цикл завершился до новой активации"
			},
			"visit beyond limit": func(s *Snapshot) {
				s.Meta.Visits = append(s.Meta.Visits, Visit{
					VisitID: newID(), StepID: "loop", Visit: 3, Iteration: 3, State: scheduler.Pending,
					Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{second.VisitID}, DecisionKey: "repeat"},
				})
			},
		} {
			t.Run(name, func(t *testing.T) {
				damaged := cloneSnapshotMetadata(t, result.Snapshot)
				mutate(&damaged)
				if err := damaged.validate(damaged.Meta.RunID); err == nil {
					t.Fatal("повреждённое доказательство maxVisits принято")
				}
			})
		}
	})

	t.Run("explicit onLimit succeeds with active visit", func(t *testing.T) {
		_, _, run, second := testBoundedLoopAtLimit(t, true)
		result, err := run.AdvanceAgentGraph()
		if err != nil || result.Snapshot.Meta.RunState != RunSucceeded || result.Snapshot.Meta.StopVisitID != second.VisitID ||
			result.Snapshot.Meta.StopLimitStepID != "loop" || result.Snapshot.Meta.StopLimitTrigger == nil ||
			result.Snapshot.Meta.StopLimitIteration != 3 || result.Plan.Terminal == nil ||
			result.Plan.Terminal.Outcome != workflow.OutcomeSucceeded || result.Snapshot.Meta.Visits[1].State != scheduler.Skipped {
			t.Fatalf("onLimit не завершил run при активной параллельной ветке: %+v, %v", result, err)
		}
	})
}

// TestAdvanceAgentGraphLimitKeepsUnselectedSkipped фиксирует связь onLimit с
// новой семантикой веток. Выбранный target N+1 не создаётся и решение остаётся
// unapplied, но известная невыбранная альтернатива всё равно обязана получить
// causal Skipped в том же terminal commit и пройти повторную Load-валидацию.
func TestAdvanceAgentGraphLimitKeepsUnselectedSkipped(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, Input{WorkflowJSON: []byte(`{
  "version":2,
  "id":"limit-keeps-skipped",
  "start":["choice","limited"],
  "steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "go":{"to":["limited"]},"other":{"to":["unused-a","unused-b"]}}},
    {"id":"limited","type":"agent","prompt":"Лимит","after":[],"maxVisits":1},
    {"id":"unused-a","type":"agent","prompt":"Не выбран A","after":[]},
    {"id":"unused-b","type":"agent","prompt":"Не выбран B","after":[]}
  ]
}`), Task: "Проверить skipped при maxVisits", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	choiceID := initial.Meta.Visits[0].VisitID
	finishPlainVisit(t, run, initial.Meta.Visits[1].VisitID)
	finishDecisionVisit(t, run, choiceID, "go")
	result, err := run.AdvanceAgentGraph()
	if err != nil || result.Snapshot.Meta.RunState != RunFailed || result.Snapshot.Meta.StopLimitStepID != "limited" ||
		len(result.CreatedVisits) != 2 ||
		result.Snapshot.Meta.Visits[0].Decision.Applied {
		t.Fatalf("onLimit потерял невыбранную ветку или применил route: %+v, %v", result, err)
	}
	createdTargets := make(map[string]bool, len(result.CreatedVisits))
	for _, visit := range result.CreatedVisits {
		createdTargets[visit.StepID] = true
		if visit.State != scheduler.Skipped || visit.Trigger.Kind != TriggerDecisionSkipped ||
			!slices.Equal(visit.Trigger.SourceVisitIDs, []string{choiceID}) {
			t.Fatalf("decision-limit создал неверную skipped-альтернативу: %+v", visit)
		}
	}
	if !createdTargets["unused-a"] || !createdTargets["unused-b"] {
		t.Fatalf("decision-limit потерял один из skipped targets: %+v", result.CreatedVisits)
	}
	if reloaded, loadErr := run.Load(); loadErr != nil || !reflect.DeepEqual(reloaded.Meta, result.Snapshot.Meta) {
		t.Fatalf("limit snapshot со skipped не прошёл Load: %+v, %v", reloaded.Meta, loadErr)
	}
	damaged := cloneSnapshotMetadata(t, result.Snapshot)
	for index, visit := range damaged.Meta.Visits {
		if visit.StepID == "unused-b" {
			damaged.Meta.Visits = slices.Delete(damaged.Meta.Visits, index, index+1)
			break
		}
	}
	if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "неполное skipped-замыкание") {
		t.Fatalf("decision-limit terminal принял удалённую skipped-альтернативу: %v", err)
	}
}

// TestAdvanceAgentGraphAfterLimitDoesNotReuseFIFO фиксирует атомарность между
// виртуальной N+1 after-активацией и terminal skipped-замыканием. Источник b
// уже входит в StopLimitTrigger вместе с ранним real a; более поздний causal a
// не вправе повторно соединиться с тем же b и создать Skipped-квитанцию
// ограниченного шага после опубликованного outcome.
func TestAdvanceAgentGraphAfterLimitDoesNotReuseFIFO(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, Input{WorkflowJSON: []byte(`{
  "version":2,"id":"after-limit-fifo","start":["choice","a","limited"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "main":{"to":["worker"]},"skipped":{"to":["a","b"]}}},
    {"id":"a","type":"agent","prompt":"Источник A","after":[]},
    {"id":"limited","type":"agent","prompt":"Лимит","after":["a","b"],"maxVisits":1},
    {"id":"worker","type":"agent","prompt":"Работа","after":[]},
    {"id":"b","type":"agent","prompt":"Источник B","after":[]}
  ]
}`), Task: "Проверить FIFO after-limit", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "main")
	finishPlainVisit(t, run, initial.Meta.Visits[1].VisitID)
	finishPlainVisit(t, run, initial.Meta.Visits[2].VisitID)

	first, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	workerID, skippedBID := "", ""
	for _, visit := range first.CreatedVisits {
		switch visit.StepID {
		case "worker":
			workerID = visit.VisitID
		case "b":
			skippedBID = visit.VisitID
		}
	}
	if workerID == "" || skippedBID == "" {
		t.Fatalf("fixture не создала selected и skipped branches: %+v", first.CreatedVisits)
	}
	finishPlainVisit(t, run, workerID)

	limited, err := run.AdvanceAgentGraph()
	if err != nil || limited.Snapshot.Meta.RunState != RunFailed || limited.Snapshot.Meta.StopLimitStepID != "limited" ||
		limited.Snapshot.Meta.StopLimitTrigger == nil ||
		!slices.Equal(limited.Snapshot.Meta.StopLimitTrigger.SourceVisitIDs, []string{initial.Meta.Visits[1].VisitID, skippedBID}) ||
		len(limited.CreatedVisits) != 0 {
		t.Fatalf("after-limit повторно использовал FIFO source в closure: %+v, %v", limited, err)
	}
	if reloaded, loadErr := run.Load(); loadErr != nil || !reflect.DeepEqual(reloaded.Meta, limited.Snapshot.Meta) {
		t.Fatalf("сохранённый after-limit не прошёл повторный Load: %+v, %v", reloaded.Meta, loadErr)
	}
}

// TestAdvanceAgentGraphLimitWaitsForDecisionWave воспроизводит гонку между
// готовой after-активацией N+1 и уже сохранённым, но ещё не завершившимся
// choose_decision. Пока Result второго агента не получен, limit не публикуется;
// после Result явный finish обязан победить независимо от скорости callback.
func TestAdvanceAgentGraphLimitWaitsForDecisionWave(t *testing.T) {
	root := t.TempDir()
	input := Input{WorkflowJSON: []byte(`{
  "version":2,"id":"limit-versus-decision","start":["first","second"],"steps":[
    {"id":"first","type":"agent","prompt":"Первый источник","after":[],"decisions":{"go":{"to":["source"]}}},
    {"id":"second","type":"agent","prompt":"Второй источник","after":[],"decisions":{"go":{"to":["source"]}}},
    {"id":"source","type":"agent","prompt":"Источник","after":[]},
    {"id":"limited","type":"agent","prompt":"Ограниченная ветка","after":["source"],"maxVisits":1},
    {"id":"choice","type":"agent","prompt":"Финальное решение","after":["source"],"decisions":{"done":{"finish":"succeeded"}}}
  ]
}`), Task: "Проверить барьер решений перед лимитом", CWD: t.TempDir()}
	initial, err := Create(root, input)
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "go")
	finishDecisionVisit(t, run, initial.Meta.Visits[1].VisitID, "go")
	firstWave, err := run.AdvanceAgentGraph()
	if err != nil || len(firstWave.CreatedVisits) != 1 || firstWave.CreatedVisits[0].StepID != "source" {
		t.Fatalf("первая волна не сериализовала общий target: %+v, %v", firstWave, err)
	}
	finishPlainVisit(t, run, firstWave.CreatedVisits[0].VisitID)

	secondWave, err := run.AdvanceAgentGraph()
	if err != nil || len(secondWave.CreatedVisits) != 3 {
		t.Fatalf("вторая волна не создала source и обе after-ветки: %+v, %v", secondWave, err)
	}
	created := make(map[string]Visit, len(secondWave.CreatedVisits))
	for _, visit := range secondWave.CreatedVisits {
		created[visit.StepID] = visit
	}
	for _, stepID := range []string{"source", "limited", "choice"} {
		if created[stepID].VisitID == "" {
			t.Fatalf("во второй волне нет шага %q: %+v", stepID, secondWave.CreatedVisits)
		}
	}
	finishPlainVisit(t, run, created["source"].VisitID)
	finishPlainVisit(t, run, created["limited"].VisitID)
	choiceChatID := runDecisionVisit(t, run, created["choice"].VisitID, "done")

	waiting, err := run.AdvanceAgentGraph()
	if err != nil || waiting.Changed || waiting.Snapshot.Meta.RunState != RunRunning || waiting.Plan.Terminal != nil {
		t.Fatalf("after-limit обошёл незавершённую decision-wave: %+v, %v", waiting, err)
	}
	if err := run.UpdateVisit(created["choice"].VisitID, scheduler.Succeeded, choiceChatID, ""); err != nil {
		t.Fatal(err)
	}
	finished, err := run.AdvanceAgentGraph()
	if err != nil || !finished.Changed || finished.Snapshot.Meta.RunState != RunSucceeded ||
		finished.Snapshot.Meta.StopVisitID != created["choice"].VisitID || finished.Snapshot.Meta.StopLimitStepID != "" ||
		finished.Snapshot.Meta.StopLimitTrigger != nil || finished.Snapshot.Meta.StopLimitIteration != 0 ||
		finished.Plan.Terminal == nil || finished.Plan.Terminal.Outcome != workflow.OutcomeSucceeded {
		t.Fatalf("завершённая decision-wave не выбрала explicit finish: %+v, %v", finished, err)
	}
}

// TestAgentLimitProofSurvivesTerminalDrain фиксирует монотонность durable
// maxVisits-proof. После публикации limit-a независимый Running source-b может
// завершиться и открыть более ранний по Workflow limit-b с другим onLimit.
// Сохранённый исход уже необратим и проверяется своей локальной причиной, а не
// повторным планированием изменившегося глобального frontier.
func TestAgentLimitProofSurvivesTerminalDrain(t *testing.T) {
	root := t.TempDir()
	input := Input{WorkflowJSON: []byte(`{
  "version":2,"id":"stable-limit-proof","start":["a1","a2","b1","b2"],"steps":[
    {"id":"a1","type":"agent","prompt":"A1","after":[],"decisions":{"go":{"to":["source-a"]}}},
    {"id":"a2","type":"agent","prompt":"A2","after":[],"decisions":{"go":{"to":["source-a"]}}},
    {"id":"b1","type":"agent","prompt":"B1","after":[],"decisions":{"go":{"to":["source-b"]}}},
    {"id":"b2","type":"agent","prompt":"B2","after":[],"decisions":{"go":{"to":["source-b"]}}},
    {"id":"source-a","type":"agent","prompt":"Источник A","after":[]},
    {"id":"source-b","type":"agent","prompt":"Источник B","after":[]},
    {"id":"limit-b","type":"agent","prompt":"Лимит B","after":["source-b"],"maxVisits":1,"onLimit":"succeeded"},
    {"id":"limit-a","type":"agent","prompt":"Лимит A","after":["source-a"],"maxVisits":1}
  ]
}`), Task: "Проверить стабильность причины лимита", CWD: t.TempDir()}
	initial, err := Create(root, input)
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	for _, visit := range initial.Meta.Visits {
		finishDecisionVisit(t, run, visit.VisitID, "go")
	}

	firstWave, err := run.AdvanceAgentGraph()
	if err != nil || len(firstWave.CreatedVisits) != 2 {
		t.Fatalf("первые общие targets не созданы: %+v, %v", firstWave, err)
	}
	for _, visit := range firstWave.CreatedVisits {
		finishPlainVisit(t, run, visit.VisitID)
	}
	secondWave, err := run.AdvanceAgentGraph()
	if err != nil || len(secondWave.CreatedVisits) != 4 {
		t.Fatalf("вторые sources и первые limit-visits не созданы: %+v, %v", secondWave, err)
	}
	created := make(map[string]Visit, len(secondWave.CreatedVisits))
	for _, visit := range secondWave.CreatedVisits {
		created[visit.StepID] = visit
	}
	for _, stepID := range []string{"source-a", "source-b", "limit-a", "limit-b"} {
		if created[stepID].VisitID == "" {
			t.Fatalf("во второй волне нет шага %q: %+v", stepID, secondWave.CreatedVisits)
		}
	}
	finishPlainVisit(t, run, created["source-a"].VisitID)
	sourceBChatID := runDecisionlessVisit(t, run, created["source-b"].VisitID)
	finishPlainVisit(t, run, created["limit-a"].VisitID)
	finishPlainVisit(t, run, created["limit-b"].VisitID)

	limited, err := run.AdvanceAgentGraph()
	if err != nil || !limited.Changed || limited.Snapshot.Meta.RunState != RunFailed ||
		limited.Snapshot.Meta.StopLimitStepID != "limit-a" || limited.Snapshot.Meta.StopLimitTrigger == nil ||
		!slices.Equal(limited.Snapshot.Meta.StopLimitTrigger.SourceVisitIDs, []string{created["source-a"].VisitID}) {
		t.Fatalf("готовый limit-a не опубликован: %+v, %v", limited, err)
	}
	if err := run.UpdateVisit(created["source-b"].VisitID, scheduler.Succeeded, sourceBChatID, ""); err != nil {
		t.Fatalf("terminal drain инвалидировал сохранённый limit-proof: %v", err)
	}
	drained, err := run.Load()
	if err != nil || drained.Meta.RunState != RunFailed || drained.Meta.StopLimitStepID != "limit-a" {
		t.Fatalf("terminal drain изменил опубликованный исход: %+v, %v", drained.Meta, err)
	}
	replanned, err := scheduler.PlanAgentGraph(drained.Workflow, agentVisitViews(drained.Meta.Visits))
	if err != nil || replanned.Terminal == nil || replanned.Terminal.LimitStepID != "limit-b" ||
		replanned.Terminal.Outcome != workflow.OutcomeSucceeded {
		t.Fatalf("fixture не открыла конкурирующий limit-b после drain: %+v, %v", replanned, err)
	}
}

// TestAgentDecisionLimitProofSurvivesTargetDrain защищает decision-proof от
// ложного полного replay. Ранний route заблокирован Running target X, поэтому
// поздний route честно достигает лимита B. После terminal X может завершиться;
// его новый Succeeded не означает, что X был свободен до outcome, и не должен
// задним числом превращать сохранённый limit B в ошибочный.
func TestAgentDecisionLimitProofSurvivesTargetDrain(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, Input{WorkflowJSON: []byte(`{
  "version":2,"id":"stable-decision-limit","start":["first","x","second","b"],"steps":[
    {"id":"first","type":"agent","prompt":"Первое решение","after":[],"decisions":{"go":{"to":["x"]}}},
    {"id":"x","type":"agent","prompt":"Активная цель","after":[],"maxVisits":1},
    {"id":"second","type":"agent","prompt":"Второе решение","after":[],"decisions":{"go":{"to":["b"]}}},
    {"id":"b","type":"agent","prompt":"Ограниченная цель","after":[],"maxVisits":1}
  ]
}`), Task: "Проверить drain decision-limit", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, initial.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "go")
	xChatID := runDecisionlessVisit(t, run, initial.Meta.Visits[1].VisitID)
	finishDecisionVisit(t, run, initial.Meta.Visits[2].VisitID, "go")
	finishPlainVisit(t, run, initial.Meta.Visits[3].VisitID)

	limited, err := run.AdvanceAgentGraph()
	if err != nil || limited.Snapshot.Meta.RunState != RunFailed || limited.Snapshot.Meta.StopLimitStepID != "b" ||
		limited.Snapshot.Meta.StopLimitTrigger == nil ||
		!slices.Equal(limited.Snapshot.Meta.StopLimitTrigger.SourceVisitIDs, []string{initial.Meta.Visits[2].VisitID}) {
		t.Fatalf("Running X не позволил честно опубликовать limit B: %+v, %v", limited, err)
	}
	if err := run.UpdateVisit(initial.Meta.Visits[1].VisitID, scheduler.Succeeded, xChatID, ""); err != nil {
		t.Fatalf("завершение X во время terminal drain инвалидировало decision-proof: %v", err)
	}
	drained, err := run.Load()
	if err != nil || drained.Meta.StopLimitStepID != "b" || drained.Meta.RunState != RunFailed {
		t.Fatalf("decision-limit изменился после drain: %+v, %v", drained.Meta, err)
	}
}

// TestAdvanceAgentGraphGuards отклоняет legacy и повреждённую историю до любой
// новой памяти или metadata.
func TestAdvanceAgentGraphGuards(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		root := t.TempDir()
		snapshot, err := Create(root, testInput(t))
		if err != nil {
			t.Fatal(err)
		}
		run, err := OpenLocked(root, snapshot.Meta.RunID)
		if err != nil {
			t.Fatal(err)
		}
		defer run.Close()
		if _, err := run.AdvanceAgentGraph(); err == nil {
			t.Fatal("legacy run принят visit-aware bridge")
		}
	})
	t.Run("tampered", func(t *testing.T) {
		root, snapshot, run := testAdvanceRun(t)
		metaPath := filepath.Join(root, snapshot.Meta.RunID, "meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatal(err)
		}
		damaged := bytes.Replace(data, []byte(`"state":"pending"`), []byte(`"state":"broken"`), 1)
		if bytes.Equal(data, damaged) {
			t.Fatal("fixture не повредила metadata")
		}
		mustWrite(t, metaPath, damaged)
		if _, err := run.AdvanceAgentGraph(); err == nil || !strings.Contains(err.Error(), "неизвестное состояние") {
			t.Fatalf("повреждённая metadata принята: %v", err)
		}
	})
}
