//go:build darwin || linux

package runstore

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// agentGraphInput содержит два стартовых посещения, три варианта решения и
// обычное after-ребро. Этого достаточно для проверки материалации маршрута,
// причин активации и независимости параллельных visits без запуска coordinator.
func agentGraphInput(t *testing.T) Input {
	t.Helper()
	return Input{
		WorkflowJSON: []byte(`{
  "version": 2,
  "id": "agent-graph",
  "start": ["choice", "audit"],
  "steps": [
    {"id":"choice","type":"agent","prompt":"Выбери маршрут","after":[],"decisions":{
      "go":{"to":["work"]},"done":{"finish":"succeeded"},"fail":{"finish":"failed"}}},
    {"id":"audit","type":"agent","prompt":"Проверь вход","after":[]},
    {"id":"work","type":"agent","prompt":"Выполни работу","after":[]},
    {"id":"check","type":"agent","prompt":"Проверь результат","after":["work"]}
  ]
}`),
		Task: "Выполнить agent graph", Comment: "Тест", CWD: t.TempDir(),
	}
}

func testAgentGraphRun(t *testing.T) (string, Snapshot, *LockedRun) {
	t.Helper()
	root := t.TempDir()
	snapshot, err := Create(root, agentGraphInput(t))
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

// TestCreateSelectsMetadataFormat проверяет production-границу форматов: единый
// Create выбирает v4 для workflow v2, но не мигрирует неявно старые определения
// без version и явный version=1. Это позволяет всем entry points вызывать один
// API, не смешивая Steps и Visits в одном run.
func TestCreateSelectsMetadataFormat(t *testing.T) {
	root, input := filepath.Join(t.TempDir(), "runs"), agentGraphInput(t)
	snapshot, err := Create(root, input)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Meta.Version != 4 || snapshot.Meta.RunState != RunRunning || snapshot.Meta.Steps != nil || len(snapshot.Meta.Visits) != 2 {
		t.Fatalf("неверная основа v4: %+v", snapshot.Meta)
	}
	if !childOccupied(snapshot) {
		t.Fatal("running v4 child ошибочно считается завершённым")
	}
	finished := snapshot
	finished.Meta.RunState = RunSucceeded
	if childOccupied(finished) {
		t.Fatal("terminal v4 child ошибочно считается занятым")
	}
	for index, visit := range snapshot.Meta.Visits {
		if visit.StepID != snapshot.Workflow.Start[index] || visit.Visit != 1 || visit.Iteration != 1 ||
			visit.Attempt != 0 || visit.State != scheduler.Pending || visit.Trigger.Kind != TriggerStart || !validID(visit.VisitID) {
			t.Fatalf("неверное начальное посещение: %+v", visit)
		}
		memory := filepath.Join(root, snapshot.Meta.RunID, "memory", visit.VisitID+".md")
		if data, err := os.ReadFile(memory); err != nil || len(data) != 0 {
			t.Fatalf("не создана память visit: %q, %v", data, err)
		}
		mustWrite(t, memory, []byte("память посещения"))
		if data, err := ReadMemory(root, snapshot.Meta.RunID, visit.VisitID); err != nil || string(data) != "память посещения" {
			t.Fatalf("ReadMemory не использует visitId: %q, %v", data, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, snapshot.Meta.RunID, "meta.json"))
	if err != nil || bytes.Contains(data, []byte(`"steps"`)) || !bytes.Contains(data, []byte(`"visits"`)) {
		t.Fatalf("v4 смешан с legacy metadata: %s, %v", data, err)
	}
	if loaded, err := Load(root, snapshot.Meta.RunID); err != nil || !reflect.DeepEqual(loaded, snapshot) {
		t.Fatalf("v4 не читается: %+v, %v", loaded, err)
	}
	withUnknown := bytes.Replace(data, []byte(`"runState":"running"`), []byte(`"runState":"running","future":true`), 1)
	mustWrite(t, filepath.Join(root, snapshot.Meta.RunID, "meta.json"), withUnknown)
	if _, err := Load(root, snapshot.Meta.RunID); err == nil {
		t.Fatal("строгий Load принял неизвестное поле v4")
	}
	withLegacyNull := bytes.Replace(data, []byte(`"runState"`), []byte(`"steps":null,"runState"`), 1)
	mustWrite(t, filepath.Join(root, snapshot.Meta.RunID, "meta.json"), withLegacyNull)
	if _, err := Load(root, snapshot.Meta.RunID); err == nil {
		t.Fatal("v4 принял даже null legacy-поле steps")
	}
	mustWrite(t, filepath.Join(root, snapshot.Meta.RunID, "meta.json"), data)
	for name, definition := range map[string][]byte{
		"version отсутствует": []byte(`{"id":"legacy-default","steps":[{"id":"work","type":"agent","prompt":"Работай","dependsOn":[]}]}`),
		"version равен 1":     []byte(`{"version":1,"id":"legacy-explicit","steps":[{"id":"work","type":"agent","prompt":"Работай","dependsOn":[]}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			legacy, createErr := Create(root, Input{WorkflowJSON: definition, Task: "Legacy", CWD: t.TempDir()})
			if createErr != nil {
				t.Fatal(createErr)
			}
			if legacy.Meta.Version != 3 || legacy.Meta.RunState != "" || legacy.Meta.Visits != nil || len(legacy.Meta.Steps) != 1 {
				t.Fatalf("legacy workflow записан не в v3: %+v", legacy.Meta)
			}
		})
	}
	if err := RemoveUnstarted(root, snapshot.Meta.RunID); err != nil {
		t.Fatalf("не начатый v4 run не удалён: %v", err)
	}
	if _, err := Load(root, snapshot.Meta.RunID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("удалённый v4 run остался доступен: %v", err)
	}
}

// TestLegacyMetadataRejectsV4Fields не позволяет старому runtime молча
// проигнорировать часть нового журнала при ручной подмене version/meta.
func TestLegacyMetadataRejectsV4Fields(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Create(root, testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Metadata){
		"runState":           func(meta *Metadata) { meta.RunState = RunRunning },
		"stopReason":         func(meta *Metadata) { meta.StopReason = "неожиданное поле" },
		"stopVisitId":        func(meta *Metadata) { meta.StopVisitID = newID() },
		"stopLimitStepId":    func(meta *Metadata) { meta.StopLimitStepID = "work" },
		"stopLimitTrigger":   func(meta *Metadata) { meta.StopLimitTrigger = &VisitTrigger{Kind: TriggerAfter} },
		"stopLimitIteration": func(meta *Metadata) { meta.StopLimitIteration = 1 },
		"visits": func(meta *Metadata) {
			meta.Visits = []Visit{{VisitID: newID(), StepID: "чужой"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			meta := snapshot.Meta
			mutate(&meta)
			data, marshalErr := json.Marshal(meta)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			mustWrite(t, filepath.Join(root, snapshot.Meta.RunID, "meta.json"), data)
			if _, loadErr := Load(root, snapshot.Meta.RunID); loadErr == nil {
				t.Fatalf("legacy metadata приняла поле v4 %s", name)
			}
		})
	}
	original, err := json.Marshal(snapshot.Meta)
	if err != nil {
		t.Fatal(err)
	}
	withNull := bytes.Replace(original, []byte(`"version":3`), []byte(`"version":3,"visits":null`), 1)
	mustWrite(t, filepath.Join(root, snapshot.Meta.RunID, "meta.json"), withNull)
	if _, err := Load(root, snapshot.Meta.RunID); err == nil {
		t.Fatal("legacy metadata приняла visits:null")
	}
}

// TestAgentGraphMetadataValidation строит допустимую историю с decision и after,
// затем независимо повреждает важные инварианты append-only журнала.
func TestAgentGraphMetadataValidation(t *testing.T) {
	_, initial, _ := testAgentGraphRun(t)
	snapshot := initial
	choice := &snapshot.Meta.Visits[0]
	choice.State, choice.CodexThreadID, choice.TurnID, choice.Attempt = scheduler.Succeeded, "chat-choice", "turn-choice", 1
	choice.Decision = &DecisionRecord{Key: "go", TurnID: "turn-choice", CallID: "call-1", To: []string{"work"}, Skipped: []string{"done", "fail"}, Applied: true}
	workID := newID()
	snapshot.Meta.Visits = append(snapshot.Meta.Visits, Visit{
		VisitID: workID, StepID: "work", Visit: 1, Iteration: 2,
		Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{choice.VisitID}, DecisionKey: "go"},
		State:   scheduler.Succeeded, CodexThreadID: "chat-work", TurnID: "turn-work", Attempt: 1,
	}, Visit{
		VisitID: newID(), StepID: "check", Visit: 1, Iteration: 2,
		Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{workID}}, State: scheduler.Pending,
	})
	if err := snapshot.validate(snapshot.Meta.RunID); err != nil {
		t.Fatalf("допустимая история не прошла проверку: %v", err)
	}
	// Failed является технически завершённым входом after, в отличие от
	// Cancelled: прерванный чат ещё можно продолжить новым turn.
	failedSource := cloneSnapshotMetadata(t, snapshot)
	failedSource.Meta.Visits[2].State = scheduler.Failed
	failedSource.Meta.Visits[2].TechnicalError = "проверка упала"
	if err := failedSource.validate(failedSource.Meta.RunID); err != nil {
		t.Fatalf("after не принял технический Failed: %v", err)
	}
	// App Server адресует turn/call внутри thread; одинаковые opaque ID в двух
	// разных чатах не являются конфликтом глобального журнала.
	scopedIDs := cloneSnapshotMetadata(t, snapshot)
	scopedIDs.Workflow.Steps = slices.Clone(scopedIDs.Workflow.Steps)
	scopedIDs.Meta.Visits[2].TurnID = "turn-choice"
	outcome := workflow.OutcomeSucceeded
	scopedIDs.Workflow.Steps[2].Decisions = map[string]workflow.Route{"done": {Finish: &outcome}}
	scopedIDs.Meta.Visits[2].Decision = &DecisionRecord{Key: "done", TurnID: "turn-choice", CallID: "call-1", Finish: &outcome}
	if err := scopedIDs.validate(scopedIDs.Meta.RunID); err != nil {
		t.Fatalf("локальные turn/call ID ошибочно потребовали глобальной уникальности: %v", err)
	}
	// Явный finish завершает весь run немедленно и остаётся главным источником
	// истины, даже если параллельное посещение ещё Pending, Running или уже
	// Cancelled. Fixture сохраняет настоящий applied finish, а не подменяет им
	// решение `go`, которое обязано материализовать target.
	finishSnapshot := func(key string, state RunState, reason string) Snapshot {
		result := cloneSnapshotMetadata(t, initial)
		choice := &result.Meta.Visits[0]
		choice.State, choice.CodexThreadID, choice.TurnID, choice.Attempt = scheduler.Succeeded, "chat-finish", "turn-finish", 1
		route := result.Workflow.Steps[0].Decisions[key]
		finish := *route.Finish
		choice.Decision = &DecisionRecord{Key: key, TurnID: "turn-finish", CallID: "call-finish", Finish: &finish, Skipped: []string{"done", "fail", "go"}, Applied: true}
		choice.Decision.Skipped = slices.DeleteFunc(choice.Decision.Skipped, func(candidate string) bool { return candidate == key })
		result.Meta.RunState, result.Meta.StopReason, result.Meta.StopVisitID = state, reason, choice.VisitID
		return result
	}
	terminalWithPending := finishSnapshot("done", RunSucceeded, "агент выбрал успешное завершение")
	if err := terminalWithPending.validate(terminalWithPending.Meta.RunID); err != nil {
		t.Fatalf("terminal run не принял оставшиеся Pending visits: %v", err)
	}
	terminalWithRunning := cloneSnapshotMetadata(t, terminalWithPending)
	terminalWithRunning.Meta.Visits[1].State = scheduler.Running
	terminalWithRunning.Meta.Visits[1].CodexThreadID = "chat-audit"
	terminalWithRunning.Meta.Visits[1].TurnID = "turn-audit"
	terminalWithRunning.Meta.Visits[1].Attempt = 1
	if err := terminalWithRunning.validate(terminalWithRunning.Meta.RunID); err != nil {
		t.Fatalf("terminal run не принял оставшееся Running-посещение: %v", err)
	}
	terminalWithCancelled := finishSnapshot("fail", RunFailed, "агент выбрал неуспешное завершение")
	terminalWithCancelled.Meta.Visits[1].State = scheduler.Cancelled
	terminalWithCancelled.Meta.Visits[1].CodexThreadID = "chat-audit"
	if err := terminalWithCancelled.validate(terminalWithCancelled.Meta.RunID); err != nil {
		t.Fatalf("terminal run не принял оставшееся Cancelled-посещение: %v", err)
	}
	// Без explicit finish итог обязан быть доказан causal frontier: техническая
	// ошибка, уже потреблённая after, не мешает успеху, а необработанный Failed
	// является единственно допустимой причиной natural failed.
	natural := cloneSnapshotMetadata(t, snapshot)
	natural.Meta.Visits[1].State, natural.Meta.Visits[1].CodexThreadID = scheduler.Succeeded, "chat-audit"
	natural.Meta.Visits[1].TurnID, natural.Meta.Visits[1].Attempt = "turn-audit", 1
	natural.Meta.Visits[3].State, natural.Meta.Visits[3].CodexThreadID = scheduler.Succeeded, "chat-check"
	natural.Meta.Visits[3].TurnID, natural.Meta.Visits[3].Attempt = "turn-check", 1
	natural.Meta.RunState, natural.Meta.StopReason = RunSucceeded, "workflow достиг естественного завершения"
	if err := natural.validate(natural.Meta.RunID); err != nil {
		t.Fatalf("доказанный natural success отклонён: %v", err)
	}
	handledFailure := cloneSnapshotMetadata(t, natural)
	handledFailure.Meta.Visits[2].State, handledFailure.Meta.Visits[2].TechnicalError = scheduler.Failed, "тест упал"
	if err := handledFailure.validate(handledFailure.Meta.RunID); err != nil {
		t.Fatalf("обработанный Failed ошибочно отравил natural success: %v", err)
	}
	naturalFailure := cloneSnapshotMetadata(t, natural)
	naturalFailure.Meta.Visits[1].State, naturalFailure.Meta.Visits[1].TechnicalError = scheduler.Failed, "аудит упал"
	naturalFailure.Meta.RunState, naturalFailure.Meta.StopReason = RunFailed, "необработанное terminal-посещение завершилось с ошибкой"
	naturalFailure.Meta.StopVisitID = naturalFailure.Meta.Visits[1].VisitID
	if err := naturalFailure.validate(naturalFailure.Meta.RunID); err != nil {
		t.Fatalf("доказанный natural failed отклонён: %v", err)
	}
	wrongNaturalCause := cloneSnapshotMetadata(t, naturalFailure)
	wrongNaturalCause.Meta.StopVisitID = wrongNaturalCause.Meta.Visits[0].VisitID
	if err := wrongNaturalCause.validate(wrongNaturalCause.Meta.RunID); err == nil {
		t.Fatal("natural failed принял не-Failed причину")
	}
	falseSuccess := cloneSnapshotMetadata(t, naturalFailure)
	falseSuccess.Meta.RunState, falseSuccess.Meta.StopReason, falseSuccess.Meta.StopVisitID = RunSucceeded, "ложный успех", ""
	if err := falseSuccess.validate(falseSuccess.Meta.RunID); err == nil {
		t.Fatal("natural succeeded принял необработанный Failed frontier")
	}
	falseSuccessCause := cloneSnapshotMetadata(t, natural)
	falseSuccessCause.Meta.StopVisitID = falseSuccessCause.Meta.Visits[0].VisitID
	if err := falseSuccessCause.validate(falseSuccessCause.Meta.RunID); err == nil {
		t.Fatal("natural succeeded принял произвольный stopVisitId")
	}
	clone := func() Snapshot {
		return cloneSnapshotMetadata(t, snapshot)
	}
	for name, mutate := range map[string]func(*Snapshot){
		"legacy steps":       func(s *Snapshot) { s.Meta.Steps = []Step{{ID: "choice"}} },
		"duplicate visit id": func(s *Snapshot) { s.Meta.Visits[2].VisitID = s.Meta.Visits[0].VisitID },
		"visit gap":          func(s *Snapshot) { s.Meta.Visits[2].Visit = 2 },
		"unknown source":     func(s *Snapshot) { s.Meta.Visits[2].Trigger.SourceVisitIDs[0] = newID() },
		"wrong after order":  func(s *Snapshot) { s.Meta.Visits[3].Trigger.SourceVisitIDs[0] = s.Meta.Visits[0].VisitID },
		"decision iteration": func(s *Snapshot) { s.Meta.Visits[2].Iteration = 1 },
		"after iteration":    func(s *Snapshot) { s.Meta.Visits[3].Iteration = 3 },
		"duplicate chat":     func(s *Snapshot) { s.Meta.Visits[2].CodexThreadID = "chat-choice" },
		"unsafe diagnostic": func(s *Snapshot) {
			s.Meta.Visits[2].State, s.Meta.Visits[2].TechnicalError = scheduler.Failed, "ошибка\x1b[31m"
		},
		"changed route": func(s *Snapshot) { s.Meta.Visits[0].Decision.To = []string{"audit"} },
		"unmaterialized route": func(s *Snapshot) {
			s.Meta.Visits = s.Meta.Visits[:2]
		},
		"unknown stop visit": func(s *Snapshot) { s.Meta.StopVisitID = newID() },
		"failed decision source": func(s *Snapshot) {
			s.Meta.Visits[0].State = scheduler.Failed
		},
		"cancelled decision source": func(s *Snapshot) {
			s.Meta.Visits[0].State = scheduler.Cancelled
		},
		"cancelled after source": func(s *Snapshot) {
			s.Meta.Visits[2].State = scheduler.Cancelled
		},
		"active duplicate": func(s *Snapshot) {
			s.Meta.Visits = append(s.Meta.Visits, Visit{VisitID: newID(), StepID: "audit", Visit: 2, Iteration: 3, State: scheduler.Pending, Trigger: VisitTrigger{Kind: TriggerAfter}})
		},
		"reused decision cause": func(s *Snapshot) {
			s.Meta.Visits = append(s.Meta.Visits, Visit{VisitID: newID(), StepID: "work", Visit: 2, Iteration: 2, State: scheduler.Pending,
				Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{s.Meta.Visits[0].VisitID}, DecisionKey: "go"}})
		},
		"reused after cause": func(s *Snapshot) {
			s.Meta.Visits[3].State, s.Meta.Visits[3].CodexThreadID = scheduler.Succeeded, "chat-check"
			s.Meta.Visits[3].TurnID, s.Meta.Visits[3].Attempt = "turn-check", 1
			s.Meta.Visits = append(s.Meta.Visits, Visit{VisitID: newID(), StepID: "check", Visit: 2, Iteration: 2, State: scheduler.Pending,
				Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{s.Meta.Visits[2].VisitID}}})
		},
		"failed without reason": func(s *Snapshot) {
			s.Meta.RunState = RunFailed
		},
	} {
		t.Run(name, func(t *testing.T) {
			damaged := clone()
			mutate(&damaged)
			if err := damaged.validate(damaged.Meta.RunID); err == nil {
				t.Fatal("повреждённая v4 metadata принята")
			}
		})
	}
	finishMismatch := cloneSnapshotMetadata(t, terminalWithPending)
	finishMismatch.Meta.RunState, finishMismatch.Meta.StopReason = RunFailed, "подменённый итог"
	if err := finishMismatch.validate(finishMismatch.Meta.RunID); err == nil {
		t.Fatalf("applied finish не связан с итоговым runState: %+v", finishMismatch.Meta.Visits[0].Decision)
	}
	overlap := cloneSnapshotMetadata(t, snapshot)
	overlap.Meta.Visits = append(overlap.Meta.Visits, Visit{VisitID: newID(), StepID: "audit", Visit: 2, Iteration: 2,
		State: scheduler.Succeeded, CodexThreadID: "chat-audit-2", TurnID: "turn-audit-2", Attempt: 1})
	if err := overlap.validate(overlap.Meta.RunID); err == nil || !strings.Contains(err.Error(), "предыдущее посещение") {
		t.Fatalf("terminal visit обошёл незавершённое предыдущее посещение: %v", err)
	}
}

// skippedJournalSnapshot собирает журнал, в котором выбранная ветка решения
// сосуществует с причинной волной пропущенных альтернатив. Вложенный decision
// распространяет ту же волну дальше, а after различает полностью пропущенный и
// смешанный набор источников. Fixture не использует runtime-планировщик: этот
// срез проверяет только durable-формат, который тот будет атомарно записывать.
func skippedJournalSnapshot(t *testing.T) Snapshot {
	t.Helper()
	snapshot, err := Create(t.TempDir(), Input{
		WorkflowJSON: []byte(`{
  "version": 2,
  "id": "skipped-journal",
  "start": ["choice", "real"],
  "steps": [
    {"id":"choice","type":"agent","prompt":"Выбери ветку","after":[],"decisions":{
      "main":{"to":["selected"]},"alpha":{"to":["inner"]},"zeta":{"to":["inner"]}}},
    {"id":"real","type":"agent","prompt":"Выполни реальную ветку","after":[]},
    {"id":"selected","type":"agent","prompt":"Выполни выбранную ветку","after":[]},
    {"id":"inner","type":"agent","prompt":"Вложенное решение","after":[],"maxVisits":2,"decisions":{
      "a":{"to":["leaf","inner"]},"b":{"to":["leaf"]}}},
    {"id":"leaf","type":"agent","prompt":"Вложенный лист","after":[]},
    {"id":"all-skipped","type":"agent","prompt":"Полностью пропущенный join","after":["inner","leaf"]},
    {"id":"mixed","type":"agent","prompt":"Смешанный join","after":["real","inner"]}
  ]
}`),
		Task: "Проверить журнал пропусков", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	choice, real := &snapshot.Meta.Visits[0], &snapshot.Meta.Visits[1]
	choice.State, choice.CodexThreadID, choice.TurnID, choice.Attempt = scheduler.Succeeded, "chat-choice", "turn-choice", 1
	choice.Decision = &DecisionRecord{
		Key: "main", TurnID: "turn-choice", CallID: "call-choice", To: []string{"selected"},
		Skipped: []string{"alpha", "zeta"}, Applied: true,
	}
	real.State, real.CodexThreadID, real.TurnID, real.Attempt = scheduler.Succeeded, "chat-real", "turn-real", 1
	selectedID, innerID, leafID := newID(), newID(), newID()
	snapshot.Meta.Visits = append(snapshot.Meta.Visits,
		Visit{
			VisitID: selectedID, StepID: "selected", Visit: 1, Iteration: 2,
			Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{choice.VisitID}, DecisionKey: "main"},
			State:   scheduler.Succeeded, CodexThreadID: "chat-selected", TurnID: "turn-selected", Attempt: 1,
		},
		Visit{
			VisitID: innerID, StepID: "inner", Visit: 1, Iteration: 2,
			Trigger: VisitTrigger{Kind: TriggerDecisionSkipped, SourceVisitIDs: []string{choice.VisitID}, DecisionKey: "alpha"},
			State:   scheduler.Skipped,
		},
		Visit{
			VisitID: leafID, StepID: "leaf", Visit: 1, Iteration: 3,
			Trigger: VisitTrigger{Kind: TriggerDecisionSkipped, SourceVisitIDs: []string{innerID}, DecisionKey: "a"},
			State:   scheduler.Skipped,
		},
		Visit{
			VisitID: newID(), StepID: "all-skipped", Visit: 1, Iteration: 3,
			Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{innerID, leafID}}, State: scheduler.Skipped,
		},
		Visit{
			VisitID: newID(), StepID: "mixed", Visit: 1, Iteration: 2,
			Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{real.VisitID, innerID}}, State: scheduler.Pending,
		},
	)
	if err := snapshot.validate(snapshot.Meta.RunID); err != nil {
		t.Fatalf("допустимый skipped-журнал не прошёл проверку: %v", err)
	}
	return snapshot
}

// TestAgentGraphSkippedJournalValidation закрепляет причинность synthetic
// Skipped. Ключ alternative каноничен, вложенная волна конечна даже на route-
// цикле, а состояние after выводится только из фактических состояний источников.
func TestAgentGraphSkippedJournalValidation(t *testing.T) {
	valid := skippedJournalSnapshot(t)
	clone := func() Snapshot { return cloneSnapshotMetadata(t, valid) }
	for name, mutate := range map[string]func(*Snapshot){
		"skipped с данными запуска": func(s *Snapshot) {
			s.Meta.Visits[3].CodexThreadID = "forged-chat"
		},
		"неканоничный ключ общей альтернативы": func(s *Snapshot) {
			s.Meta.Visits[3].Trigger.DecisionKey = "zeta"
		},
		"decision_skipped не является skipped": func(s *Snapshot) {
			s.Meta.Visits[3].State = scheduler.Pending
		},
		"all-skipped after запущен": func(s *Snapshot) {
			s.Meta.Visits[5].State = scheduler.Pending
		},
		"mixed after пропущен": func(s *Snapshot) {
			s.Meta.Visits[6].State = scheduler.Skipped
		},
		"route-цикл повторно достигнут той же волной": func(s *Snapshot) {
			s.Meta.Visits = append(s.Meta.Visits, Visit{
				VisitID: newID(), StepID: "inner", Visit: 2, Iteration: 3,
				Trigger: VisitTrigger{Kind: TriggerDecisionSkipped, SourceVisitIDs: []string{s.Meta.Visits[3].VisitID}, DecisionKey: "a"},
				State:   scheduler.Skipped,
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			damaged := clone()
			mutate(&damaged)
			if err := damaged.validate(damaged.Meta.RunID); err == nil {
				t.Fatal("повреждённый skipped-журнал принят")
			}
		})
	}
}

// TestAgentGraphSkippedDoesNotConsumeMaxVisits проверяет общий target двух
// решений. Synthetic-запись сохраняет собственный ordinal visit, но не занимает
// квоту и не мешает выбранной ветке создать единственный реальный запуск.
func TestAgentGraphSkippedDoesNotConsumeMaxVisits(t *testing.T) {
	snapshot, err := Create(t.TempDir(), Input{
		WorkflowJSON: []byte(`{
  "version": 2,
  "id": "skipped-quota",
  "start": ["skip", "run"],
  "steps": [
    {"id":"skip","type":"agent","prompt":"Пропусти target","after":[],"decisions":{
      "main":{"to":["other"]},"unused":{"to":["target"]}}},
    {"id":"run","type":"agent","prompt":"Выбери target","after":[],"decisions":{"go":{"to":["target"]}}},
    {"id":"other","type":"agent","prompt":"Другая ветка","after":[]},
    {"id":"target","type":"agent","prompt":"Общий target","after":[],"maxVisits":1}
  ]
}`),
		Task: "Проверить квоту пропуска", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	skip, run := &snapshot.Meta.Visits[0], &snapshot.Meta.Visits[1]
	skip.State, skip.CodexThreadID, skip.TurnID, skip.Attempt = scheduler.Succeeded, "chat-skip", "turn-skip", 1
	skip.Decision = &DecisionRecord{Key: "main", TurnID: "turn-skip", CallID: "call-skip", To: []string{"other"}, Skipped: []string{"unused"}, Applied: true}
	run.State, run.CodexThreadID, run.TurnID, run.Attempt = scheduler.Succeeded, "chat-run", "turn-run", 1
	run.Decision = &DecisionRecord{Key: "go", TurnID: "turn-run", CallID: "call-run", To: []string{"target"}, Applied: true}
	snapshot.Meta.Visits = append(snapshot.Meta.Visits,
		Visit{
			VisitID: newID(), StepID: "other", Visit: 1, Iteration: 2,
			Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{skip.VisitID}, DecisionKey: "main"}, State: scheduler.Pending,
		},
		Visit{
			VisitID: newID(), StepID: "target", Visit: 1, Iteration: 2,
			Trigger: VisitTrigger{Kind: TriggerDecisionSkipped, SourceVisitIDs: []string{skip.VisitID}, DecisionKey: "unused"}, State: scheduler.Skipped,
		},
		Visit{
			VisitID: newID(), StepID: "target", Visit: 2, Iteration: 2,
			Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{run.VisitID}, DecisionKey: "go"}, State: scheduler.Pending,
		},
	)
	if err := snapshot.validate(snapshot.Meta.RunID); err != nil {
		t.Fatalf("Skipped ошибочно занял maxVisits общего target: %v", err)
	}
	overLimit := cloneSnapshotMetadata(t, snapshot)
	overLimit.Meta.Visits[4].State = scheduler.Succeeded
	overLimit.Meta.Visits[4].CodexThreadID, overLimit.Meta.Visits[4].TurnID, overLimit.Meta.Visits[4].Attempt = "chat-target", "turn-target", 1
	overLimit.Meta.Visits = append(overLimit.Meta.Visits, Visit{
		VisitID: newID(), StepID: "target", Visit: 3, Iteration: 3,
		Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{run.VisitID}, DecisionKey: "go"}, State: scheduler.Pending,
	})
	if err := overLimit.validate(overLimit.Meta.RunID); err == nil || !strings.Contains(err.Error(), "maxVisits") {
		t.Fatalf("второй реальный запуск общего target обошёл квоту: %v", err)
	}
}

// TestCanonicalSkippedTargetKey не позволяет пропустить target, который есть у
// выбранного route, даже если на него указывает и невыбранная альтернатива.
func TestCanonicalSkippedTargetKey(t *testing.T) {
	step := workflow.Step{Decisions: map[string]workflow.Route{
		"main":  {To: []string{"shared"}},
		"alpha": {To: []string{"shared", "only-skipped"}},
		"zeta":  {To: []string{"only-skipped"}},
	}}
	steps := map[string]workflow.Step{"choice": step}
	source := Visit{StepID: "choice", Decision: &DecisionRecord{
		Key: "main", To: []string{"shared"}, Skipped: []string{"alpha", "zeta"},
	}}
	if key, ok := canonicalSkippedTargetKey(source, "shared", steps); ok || key != "" {
		t.Fatalf("выбранный общий target помечен как Skipped ключом %q", key)
	}
	if key, ok := canonicalSkippedTargetKey(source, "only-skipped", steps); !ok || key != "alpha" {
		t.Fatalf("не выбран канонический ключ общей альтернативы: %q, %v", key, ok)
	}
}

// TestAgentGraphSkippedValidation проверяет, что новый terminal state нельзя
// подделать внешними ID или оторвать от невыбранного route. Отсутствующий
// synthetic token старого v4 допустим для lazy backfill, но любая уже
// присутствующая запись обязана иметь точную и единственную причину.
func TestAgentGraphSkippedValidation(t *testing.T) {
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
	finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "left")
	advanced, err := run.AdvanceAgentGraph()
	if err != nil {
		t.Fatal(err)
	}
	if err = advanced.Snapshot.validate(initial.Meta.RunID); err != nil {
		t.Fatalf("допустимый Skipped snapshot отклонён: %v", err)
	}
	rightIndex := -1
	for index, visit := range advanced.Snapshot.Meta.Visits {
		if visit.StepID == "right" {
			rightIndex = index
			break
		}
	}
	if rightIndex < 0 {
		t.Fatal("planner не создал Skipped right")
	}

	for name, mutate := range map[string]func(*Snapshot){
		"codex payload": func(snapshot *Snapshot) {
			snapshot.Meta.Visits[rightIndex].CodexThreadID = "chat-skipped"
		},
		"selected key": func(snapshot *Snapshot) {
			snapshot.Meta.Visits[rightIndex].Trigger.DecisionKey = "left"
		},
		"non-terminal state": func(snapshot *Snapshot) {
			snapshot.Meta.Visits[rightIndex].State = scheduler.Pending
		},
		"duplicate cause": func(snapshot *Snapshot) {
			duplicate := snapshot.Meta.Visits[rightIndex]
			duplicate.VisitID, duplicate.Visit = newID(), duplicate.Visit+1
			snapshot.Meta.Visits = append(snapshot.Meta.Visits, duplicate)
		},
	} {
		t.Run(name, func(t *testing.T) {
			damaged := cloneSnapshotMetadata(t, advanced.Snapshot)
			mutate(&damaged)
			if err := damaged.validate(damaged.Meta.RunID); err == nil {
				t.Fatal("повреждённый Skipped snapshot принят")
			}
		})
	}

	left := advanced.CreatedVisits[0]
	finishPlainVisit(t, run, left.VisitID)
	joined, err := run.AdvanceAgentGraph()
	if err != nil || len(joined.CreatedVisits) != 1 {
		t.Fatalf("не создан mixed join: %+v, %v", joined, err)
	}
	mixed := cloneSnapshotMetadata(t, joined.Snapshot)
	mixed.Meta.Visits[len(mixed.Meta.Visits)-1].State = scheduler.Skipped
	if err := mixed.validate(mixed.Meta.RunID); err == nil {
		t.Fatal("работающий graph принял Skipped after со смешанными источниками")
	}

	startSkipped := cloneSnapshotMetadata(t, initial)
	startSkipped.Meta.Visits[0].State = scheduler.Skipped
	if err := startSkipped.validate(startSkipped.Meta.RunID); err == nil {
		t.Fatal("работающий graph принял произвольно пропущенный start")
	}
	forgedNatural := cloneSnapshotMetadata(t, startSkipped)
	forgedNatural.Meta.RunState = RunSucceeded
	forgedNatural.Meta.StopReason = "подделанный естественный успех"
	if err := forgedNatural.validate(forgedNatural.Meta.RunID); err == nil || !strings.Contains(err.Error(), "необоснованный skipped") {
		t.Fatalf("terminal snapshot скрыл невыполненный start под natural success: %v", err)
	}
}

// TestAgentGraphNaturalTerminalRejectsUnappliedDecision не даёт ручной подмене
// runState скрыть обязательный переход decision. Проверяются все три состояния
// готового turn: нет choose_decision, выбран обычный route и выбран finish.
func TestAgentGraphNaturalTerminalRejectsUnappliedDecision(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, terminalNestedSkippedInput(t))
	if err != nil {
		t.Fatal(err)
	}
	successful := workflow.OutcomeSucceeded
	for _, variant := range []struct {
		name     string
		decision *DecisionRecord
	}{
		{name: "missing choice"},
		{name: "route", decision: &DecisionRecord{
			Key: "investigate", TurnID: "turn-choice", CallID: "call-choice", To: []string{"inner"}, Skipped: []string{"safe"},
		}},
		{name: "finish", decision: &DecisionRecord{
			Key: "safe", TurnID: "turn-choice", CallID: "call-choice", Finish: &successful, Skipped: []string{"investigate"},
		}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			damaged := cloneSnapshotMetadata(t, initial)
			visit := &damaged.Meta.Visits[0]
			visit.State, visit.CodexThreadID, visit.TurnID, visit.Attempt = scheduler.Succeeded, "chat-choice", "turn-choice", 1
			visit.Decision = variant.decision
			damaged.Meta.RunState = RunSucceeded
			damaged.Meta.StopReason = "подделанный natural outcome"
			if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "неприменённое решение") {
				t.Fatalf("natural terminal скрыл обязательный decision-переход: %v", err)
			}
		})
	}
}

// TestAgentGraphNaturalTerminalRequiresPlannerQuiescence защищает causal
// frontier, который нельзя доказать одним поиском active visits. Завершённый
// source уже делает recovery-after готовым: и при успехе, и при техническом
// Failed planner обязан сначала создать downstream visit, а не публиковать
// natural outcome ручной заменой runState.
func TestAgentGraphNaturalTerminalRequiresPlannerQuiescence(t *testing.T) {
	root := t.TempDir()
	initial, err := Create(root, Input{WorkflowJSON: []byte(`{
  "version":2,
  "id":"natural-quiescence",
  "start":["source"],
  "steps":[
    {"id":"source","type":"agent","prompt":"Источник","after":[]},
    {"id":"recovery","type":"agent","prompt":"Продолжи","after":["source"]}
  ]
}`), Task: "Проверить natural quiescence", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []struct {
		name     string
		state    scheduler.State
		runState RunState
	}{
		{name: "ready after before success", state: scheduler.Succeeded, runState: RunSucceeded},
		{name: "ready recovery before failure", state: scheduler.Failed, runState: RunFailed},
	} {
		t.Run(variant.name, func(t *testing.T) {
			damaged := cloneSnapshotMetadata(t, initial)
			visit := &damaged.Meta.Visits[0]
			visit.State, visit.CodexThreadID = variant.state, "chat-source"
			if variant.state == scheduler.Succeeded {
				visit.TurnID, visit.Attempt = "turn-source", 1
			} else {
				visit.TechnicalError = "источник завершился с ошибкой"
				damaged.Meta.StopVisitID = visit.VisitID
			}
			damaged.Meta.RunState = variant.runState
			damaged.Meta.StopReason = "подделанный natural outcome"
			if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "готовую активацию") {
				t.Fatalf("natural terminal скрыл готовый after: %v", err)
			}
		})
	}
}

// TestAgentGraphNaturalTerminalDistinguishesLegacySkippedBackfill сохраняет
// чтение старых v4 snapshots, созданных до synthetic Skipped. Если в истории
// вообще нет Skipped, отсутствующая ветка может требовать только ленивый
// backfill и не инвалидирует уже опубликованный natural outcome. Но первая же
// сохранённая causal-запись включает новый контракт: её транзитивное замыкание
// обязано быть полным, иначе Load принял бы оборванную ветку за завершённую.
func TestAgentGraphNaturalTerminalDistinguishesLegacySkippedBackfill(t *testing.T) {
	initial, err := Create(t.TempDir(), nestedSkippedInput(t))
	if err != nil {
		t.Fatal(err)
	}
	legacy := cloneSnapshotMetadata(t, initial)
	outer := &legacy.Meta.Visits[0]
	outer.State, outer.CodexThreadID, outer.TurnID, outer.Attempt = scheduler.Succeeded, "chat-outer", "turn-outer", 1
	outer.Decision = &DecisionRecord{
		Key: "right", TurnID: "turn-outer", CallID: "call-outer", To: []string{"right"},
		Skipped: []string{"nested"}, Applied: true,
	}
	rightID := newID()
	legacy.Meta.Visits = append(legacy.Meta.Visits, Visit{
		VisitID: rightID, StepID: "right", Visit: 1, Iteration: 2,
		Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{outer.VisitID}, DecisionKey: "right"},
		State:   scheduler.Succeeded, CodexThreadID: "chat-right", TurnID: "turn-right", Attempt: 1,
	})
	legacy.Meta.RunState, legacy.Meta.StopReason = RunSucceeded, "старый natural outcome без skipped-backfill"
	if err := legacy.validate(legacy.Meta.RunID); err != nil {
		t.Fatalf("legacy snapshot без Skipped ошибочно отклонён: %v", err)
	}

	partial := cloneSnapshotMetadata(t, legacy)
	partial.Meta.Visits = append(partial.Meta.Visits, Visit{
		VisitID: newID(), StepID: "inner", Visit: 1, Iteration: 2, State: scheduler.Skipped,
		Trigger: VisitTrigger{Kind: TriggerDecisionSkipped, SourceVisitIDs: []string{outer.VisitID}, DecisionKey: "nested"},
	})
	if err := partial.validate(partial.Meta.RunID); err == nil || !strings.Contains(err.Error(), "неполное skipped-замыкание") {
		t.Fatalf("новая история приняла только первый слой skipped-замыкания: %v", err)
	}
}

// TestAgentGraphDecisionTerminalPriority не позволяет terminal metadata
// переставить события внутри decision-wave. Finish ждёт закрытия всех decision
// visits и выигрывает до обычных routes; fatal outcome, напротив, выбирается
// немедленно и обязан ссылаться на самую раннюю durable ошибку.
func TestAgentGraphDecisionTerminalPriority(t *testing.T) {
	input := func(t *testing.T) Input {
		t.Helper()
		return Input{WorkflowJSON: []byte(`{
  "version":2,"id":"decision-terminal-priority","start":["first","second"],"steps":[
    {"id":"first","type":"agent","prompt":"Первое решение","after":[],"decisions":{
      "done":{"finish":"succeeded"},"go":{"to":["first-target"]}}},
    {"id":"second","type":"agent","prompt":"Второе решение","after":[],"decisions":{
      "done":{"finish":"succeeded"},"go":{"to":["second-target"]}}},
    {"id":"first-target","type":"agent","prompt":"Первая цель","after":[]},
    {"id":"second-target","type":"agent","prompt":"Вторая цель","after":[]}
  ]
}`), Task: "Проверить порядок terminal decision", CWD: t.TempDir()}
	}
	outcome := workflow.OutcomeSucceeded
	for _, variant := range []struct {
		name      string
		configure func(*Snapshot)
		want      string
	}{
		{name: "earlier running decision", want: "ещё не завершило", configure: func(snapshot *Snapshot) {
			first := &snapshot.Meta.Visits[0]
			first.State, first.CodexThreadID, first.TurnID, first.Attempt = scheduler.Running, "chat-first", "turn-first", 1
		}},
		{name: "earlier missing choice", want: "без обязательного решения", configure: func(snapshot *Snapshot) {
			first := &snapshot.Meta.Visits[0]
			first.State, first.CodexThreadID, first.TurnID, first.Attempt = scheduler.Succeeded, "chat-first", "turn-first", 1
		}},
		{name: "earlier finish", want: "planner первым выбрал бы", configure: func(snapshot *Snapshot) {
			first := &snapshot.Meta.Visits[0]
			first.State, first.CodexThreadID, first.TurnID, first.Attempt = scheduler.Succeeded, "chat-first", "turn-first", 1
			first.Decision = &DecisionRecord{
				Key: "done", TurnID: "turn-first", CallID: "call-first", Finish: &outcome, Skipped: []string{"go"},
			}
		}},
		{name: "ordinary route applied after finisher existed", want: "материализовано после terminal source", configure: func(snapshot *Snapshot) {
			first := &snapshot.Meta.Visits[0]
			first.State, first.CodexThreadID, first.TurnID, first.Attempt = scheduler.Succeeded, "chat-first", "turn-first", 1
			first.Decision = &DecisionRecord{
				Key: "go", TurnID: "turn-first", CallID: "call-first", To: []string{"first-target"},
				Skipped: []string{"done"}, Applied: true,
			}
			snapshot.Meta.Visits = append(snapshot.Meta.Visits, Visit{
				VisitID: newID(), StepID: "first-target", Visit: 1, Iteration: 2, State: scheduler.Skipped,
				Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{first.VisitID}, DecisionKey: "go"},
			})
		}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			initial, err := Create(t.TempDir(), input(t))
			if err != nil {
				t.Fatal(err)
			}
			damaged := cloneSnapshotMetadata(t, initial)
			second := &damaged.Meta.Visits[1]
			second.State, second.CodexThreadID, second.TurnID, second.Attempt = scheduler.Succeeded, "chat-second", "turn-second", 1
			second.Decision = &DecisionRecord{
				Key: "done", TurnID: "turn-second", CallID: "call-second", Finish: &outcome,
				Skipped: []string{"go"}, Applied: true,
			}
			damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
				RunSucceeded, "подделанный поздний finish", second.VisitID
			variant.configure(&damaged)
			if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), variant.want) {
				t.Fatalf("terminal snapshot нарушил decision priority (%s): %v", variant.want, err)
			}
		})
	}

	for _, variant := range []struct {
		name   string
		finish bool
	}{
		{name: "finish source already advanced", finish: true},
		{name: "fatal source already advanced"},
	} {
		t.Run(variant.name, func(t *testing.T) {
			initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"terminal-source-use","start":["decision"],"steps":[
    {"id":"decision","type":"agent","prompt":"Решение","after":[],"decisions":{"done":{"finish":"succeeded"}}},
    {"id":"downstream","type":"agent","prompt":"Не должен появиться","after":["decision"]}
  ]
}`), Task: "Проверить terminal source use", CWD: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			damaged := cloneSnapshotMetadata(t, initial)
			decision := &damaged.Meta.Visits[0]
			decision.State, decision.CodexThreadID, decision.TurnID, decision.Attempt =
				scheduler.Succeeded, "chat-decision", "turn-decision", 1
			if variant.finish {
				decision.Decision = &DecisionRecord{
					Key: "done", TurnID: "turn-decision", CallID: "call-decision", Finish: &outcome, Applied: true,
				}
				damaged.Meta.RunState = RunSucceeded
			} else {
				damaged.Meta.RunState = RunFailed
			}
			damaged.Meta.StopReason, damaged.Meta.StopVisitID = "подделанный terminal source use", decision.VisitID
			damaged.Meta.Visits = append(damaged.Meta.Visits, Visit{
				VisitID: newID(), StepID: "downstream", Visit: 1, Iteration: 1, State: scheduler.Succeeded,
				Trigger:       VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{decision.VisitID}},
				CodexThreadID: "chat-downstream", TurnID: "turn-downstream", Attempt: 1,
			})
			if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "terminal decision source") {
				t.Fatalf("terminal decision успело создать downstream visit: %v", err)
			}
		})
	}

	t.Run("earlier durable conflict", func(t *testing.T) {
		initial, err := Create(t.TempDir(), input(t))
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		first := &damaged.Meta.Visits[0]
		first.State, first.CodexThreadID, first.TurnID, first.Attempt = scheduler.Running, "chat-first", "turn-first", 1
		first.Decision = &DecisionRecord{
			Key: "go", TurnID: "turn-first", CallID: "call-first", To: []string{"first-target"},
			Skipped: []string{"done"}, Error: "два разных результата одного call",
		}
		second := &damaged.Meta.Visits[1]
		second.State, second.CodexThreadID, second.TurnID, second.Attempt = scheduler.Succeeded, "chat-second", "turn-second", 1
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
			RunFailed, "подделана более поздняя ошибка решения", second.VisitID
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "planner первым выбрал бы") {
			t.Fatalf("fatal outcome обошёл ранний durable conflict: %v", err)
		}
	})

	t.Run("causal missing choice before fatal", func(t *testing.T) {
		initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"causal-missing-before-fatal","start":["first"],"steps":[
    {"id":"first","type":"agent","prompt":"Первое решение","after":[],"decisions":{"go":{"to":["target"]}}},
    {"id":"second","type":"agent","prompt":"Причинно поздняя ошибка","after":["first"],"decisions":{"go":{"to":["target"]}}},
    {"id":"target","type":"agent","prompt":"Цель","after":[]}
  ]
}`), Task: "Проверить causal missing choice", CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		first := &damaged.Meta.Visits[0]
		first.State, first.CodexThreadID, first.TurnID, first.Attempt =
			scheduler.Succeeded, "chat-first", "turn-first", 1
		secondID := newID()
		damaged.Meta.Visits = append(damaged.Meta.Visits, Visit{
			VisitID: secondID, StepID: "second", Visit: 1, Iteration: 1,
			Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{first.VisitID}},
			State:   scheduler.Succeeded, CodexThreadID: "chat-second", TurnID: "turn-second", Attempt: 1,
			Decision: &DecisionRecord{
				Key: "go", TurnID: "turn-second", CallID: "call-second", To: []string{"target"},
				Error: "конфликт durable decision",
			},
		})
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
			RunFailed, "подделана причинно поздняя ошибка решения", secondID
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "породило downstream") {
			t.Fatalf("fatal outcome обошёл причинно более ранний missing-choice: %v", err)
		}
	})

	t.Run("causal missing choice after fatal source", func(t *testing.T) {
		initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"late-causal-missing-before-fatal","start":["first","second"],"steps":[
    {"id":"first","type":"agent","prompt":"Первая ошибка","after":[],"decisions":{"go":{"to":["target"]}}},
    {"id":"second","type":"agent","prompt":"Второе решение","after":[],"decisions":{"go":{"to":["target"]}}},
    {"id":"child","type":"agent","prompt":"Недопустимый потомок","after":["second"]},
    {"id":"target","type":"agent","prompt":"Цель","after":[]}
  ]
}`), Task: "Проверить missing choice после stop visit", CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		first := &damaged.Meta.Visits[0]
		first.State, first.CodexThreadID, first.TurnID, first.Attempt =
			scheduler.Running, "chat-first", "turn-first", 1
		first.Decision = &DecisionRecord{
			Key: "go", TurnID: "turn-first", CallID: "call-first", To: []string{"target"},
			Error: "конфликт durable decision",
		}
		second := &damaged.Meta.Visits[1]
		second.State, second.CodexThreadID, second.TurnID, second.Attempt =
			scheduler.Succeeded, "chat-second", "turn-second", 1
		damaged.Meta.Visits = append(damaged.Meta.Visits, Visit{
			VisitID: newID(), StepID: "child", Visit: 1, Iteration: 1,
			Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{second.VisitID}},
			State:   scheduler.Succeeded, CodexThreadID: "chat-child", TurnID: "turn-child", Attempt: 1,
		})
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
			RunFailed, "durable conflict первого решения", first.VisitID
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "породило downstream") {
			t.Fatalf("fatal source скрыл поздний causal missing-choice: %v", err)
		}
	})

	t.Run("late missing choice during drain", func(t *testing.T) {
		initial, err := Create(t.TempDir(), input(t))
		if err != nil {
			t.Fatal(err)
		}
		terminal := cloneSnapshotMetadata(t, initial)
		first := &terminal.Meta.Visits[0]
		first.State, first.CodexThreadID, first.TurnID, first.Attempt = scheduler.Running, "chat-first", "turn-first", 1
		second := &terminal.Meta.Visits[1]
		second.State, second.CodexThreadID, second.TurnID, second.Attempt = scheduler.Running, "chat-second", "turn-second", 1
		second.Decision = &DecisionRecord{
			Key: "go", TurnID: "turn-second", CallID: "call-second", To: []string{"second-target"},
			Skipped: []string{"done"}, Error: "конфликт durable decision",
		}
		terminal.Meta.RunState, terminal.Meta.StopReason, terminal.Meta.StopVisitID =
			RunFailed, "durable conflict второго решения", second.VisitID
		if err := terminal.validate(terminal.Meta.RunID); err != nil {
			t.Fatalf("исходный fatal snapshot отклонён: %v", err)
		}
		drained := cloneSnapshotMetadata(t, terminal)
		drained.Meta.Visits[0].State = scheduler.Succeeded
		if err := drained.validate(drained.Meta.RunID); err != nil {
			t.Fatalf("поздний missing-choice инвалидировал уже опубликованный fatal outcome: %v", err)
		}
	})
}

// TestAgentGraphLimitPriorityValidation проверяет три независимых доказательства
// maxVisits: reservations более ранних решений, порядок after по Workflow.Steps
// и запрет причинной цепочки, которая возникла только из terminal cleanup.
func TestAgentGraphLimitPriorityValidation(t *testing.T) {
	t.Run("terminal cleanup obeys pre-terminal capacity", func(t *testing.T) {
		build := func(t *testing.T, firstLimit int) Snapshot {
			t.Helper()
			initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(fmt.Sprintf(`{
  "version":2,"id":"cleanup-capacity","start":["dep-a","dep-b","limit-a","limit-b"],"steps":[
    {"id":"dep-a","type":"agent","prompt":"Источник A","after":[]},
    {"id":"dep-b","type":"agent","prompt":"Источник B","after":[]},
    {"id":"limit-a","type":"agent","prompt":"Первый лимит","after":["dep-a"],"maxVisits":%d,"onLimit":"succeeded"},
    {"id":"limit-b","type":"agent","prompt":"Второй лимит","after":["dep-b"],"maxVisits":1}
  ]
}`, firstLimit)), Task: "Проверить capacity terminal cleanup", CWD: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			damaged := cloneSnapshotMetadata(t, initial)
			for index := range damaged.Meta.Visits {
				visit := &damaged.Meta.Visits[index]
				visit.State, visit.CodexThreadID, visit.TurnID, visit.Attempt =
					scheduler.Succeeded, fmt.Sprintf("chat-%d", index), fmt.Sprintf("turn-%d", index), 1
			}
			depA, depB := damaged.Meta.Visits[0], damaged.Meta.Visits[1]
			damaged.Meta.Visits = append(damaged.Meta.Visits, Visit{
				VisitID: newID(), StepID: "limit-a", Visit: 2, Iteration: 1, State: scheduler.Skipped,
				Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{depA.VisitID}},
			})
			trigger := VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{depB.VisitID}}
			damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
				RunFailed, "limit B после pre-terminal cleanup", damaged.Meta.Visits[3].VisitID
			damaged.Meta.StopLimitStepID, damaged.Meta.StopLimitTrigger, damaged.Meta.StopLimitIteration =
				"limit-b", &trigger, 1
			return damaged
		}

		forged := build(t, 1)
		if err := forged.validate(forged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "pre-terminal maxVisits=1") {
			t.Fatalf("cleanup сверх квоты скрыл более ранний limit: %v", err)
		}
		valid := build(t, 2)
		if err := valid.validate(valid.Meta.RunID); err != nil {
			t.Fatalf("допустимый Pending cleanup ошибочно отклонён: %v", err)
		}
	})

	t.Run("earlier decision reserves shared target", func(t *testing.T) {
		initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"decision-limit-priority","start":["first","second","limited"],"steps":[
    {"id":"first","type":"agent","prompt":"Первый","after":[],"decisions":{"go":{"to":["shared"]}}},
    {"id":"second","type":"agent","prompt":"Второй","after":[],"decisions":{"go":{"to":["shared","limited"]}}},
    {"id":"limited","type":"agent","prompt":"Лимит","after":[],"maxVisits":1},
    {"id":"shared","type":"agent","prompt":"Общая цель","after":[]}
  ]
}`), Task: "Проверить decision-limit priority", CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		for index, targets := range [][]string{{"shared"}, {"shared", "limited"}} {
			visit := &damaged.Meta.Visits[index]
			visit.State, visit.CodexThreadID, visit.TurnID, visit.Attempt =
				scheduler.Succeeded, fmt.Sprintf("chat-%d", index), fmt.Sprintf("turn-%d", index), 1
			visit.Decision = &DecisionRecord{
				Key: "go", TurnID: visit.TurnID, CallID: fmt.Sprintf("call-%d", index), To: targets,
			}
		}
		limited := &damaged.Meta.Visits[2]
		limited.State, limited.CodexThreadID, limited.TurnID, limited.Attempt = scheduler.Succeeded, "chat-limited", "turn-limited", 1
		trigger := VisitTrigger{
			Kind: TriggerDecision, SourceVisitIDs: []string{damaged.Meta.Visits[1].VisitID}, DecisionKey: "go",
		}
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID = RunFailed, "подделанный decision-limit", limited.VisitID
		damaged.Meta.StopLimitStepID, damaged.Meta.StopLimitTrigger, damaged.Meta.StopLimitIteration = "limited", &trigger, 2
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "порядок planner") {
			t.Fatalf("decision-limit обошёл reservation раннего route: %v", err)
		}
	})

	t.Run("after descendant created after decision quota was free", func(t *testing.T) {
		initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"decision-limit-causal-priority","start":["source","limited"],"steps":[
    {"id":"source","type":"agent","prompt":"Источник решения","after":[],"decisions":{"go":{"to":["limited"]}}},
    {"id":"limited","type":"agent","prompt":"Лимит","after":[],"maxVisits":1},
    {"id":"later","type":"agent","prompt":"Позднее решение","after":["limited"],"decisions":{"go":{"to":["worker"]}}},
    {"id":"worker","type":"agent","prompt":"Работа","after":[]}
  ]
}`), Task: "Проверить causal decision-limit", CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		source := &damaged.Meta.Visits[0]
		source.State, source.CodexThreadID, source.TurnID, source.Attempt = scheduler.Succeeded, "chat-source", "turn-source", 1
		source.Decision = &DecisionRecord{Key: "go", TurnID: "turn-source", CallID: "call-source", To: []string{"limited"}}
		limited := &damaged.Meta.Visits[1]
		limited.State, limited.CodexThreadID, limited.TurnID, limited.Attempt = scheduler.Succeeded, "chat-limited", "turn-limited", 1
		laterID, workerID := newID(), newID()
		damaged.Meta.Visits = append(damaged.Meta.Visits,
			Visit{VisitID: laterID, StepID: "later", Visit: 1, Iteration: 1, State: scheduler.Succeeded,
				Trigger:       VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{limited.VisitID}},
				CodexThreadID: "chat-later", TurnID: "turn-later", Attempt: 1,
				Decision: &DecisionRecord{Key: "go", TurnID: "turn-later", CallID: "call-later", To: []string{"worker"}, Applied: true}},
			Visit{VisitID: workerID, StepID: "worker", Visit: 1, Iteration: 2, State: scheduler.Succeeded,
				Trigger:       VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{laterID}, DecisionKey: "go"},
				CodexThreadID: "chat-worker", TurnID: "turn-worker", Attempt: 1},
		)
		trigger := VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{source.VisitID}, DecisionKey: "go"}
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID = RunFailed, "подделанный decision-limit", limited.VisitID
		damaged.Meta.StopLimitStepID, damaged.Meta.StopLimitTrigger, damaged.Meta.StopLimitIteration = "limited", &trigger, 2
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "создано после доказанного decision-limit") {
			t.Fatalf("decision-limit принял невозможного causal-потомка: %v", err)
		}
	})

	t.Run("causal descendant proves earlier decision limit", func(t *testing.T) {
		initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"earlier-causal-decision-limit","start":["decision","target","ready","limited"],"steps":[
    {"id":"decision","type":"agent","prompt":"Раннее решение","after":[],"decisions":{"again":{"to":["target"]}}},
    {"id":"target","type":"agent","prompt":"Ранняя квота","after":[],"maxVisits":1,"onLimit":"succeeded"},
    {"id":"target-child","type":"agent","prompt":"Доказательство завершения target","after":["target"]},
    {"id":"ready","type":"agent","prompt":"Источник позднего решения","after":[]},
    {"id":"late-decision","type":"agent","prompt":"Позднее решение","after":["ready"],"decisions":{"stop":{"to":["limited"]}}},
    {"id":"limited","type":"agent","prompt":"Поздняя квота","after":[],"maxVisits":1}
  ]
}`), Task: "Проверить causal proof раннего decision-limit", CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		for index := range damaged.Meta.Visits {
			visit := &damaged.Meta.Visits[index]
			visit.State, visit.CodexThreadID, visit.TurnID, visit.Attempt =
				scheduler.Succeeded, fmt.Sprintf("chat-%d", index), fmt.Sprintf("turn-%d", index), 1
		}
		decision := &damaged.Meta.Visits[0]
		decision.Decision = &DecisionRecord{
			Key: "again", TurnID: decision.TurnID, CallID: "call-decision", To: []string{"target"},
		}
		target, ready := damaged.Meta.Visits[1], damaged.Meta.Visits[2]
		childID, lateDecisionID := newID(), newID()
		damaged.Meta.Visits = append(damaged.Meta.Visits,
			Visit{VisitID: childID, StepID: "target-child", Visit: 1, Iteration: 1,
				Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{target.VisitID}},
				State:   scheduler.Succeeded, CodexThreadID: "chat-child", TurnID: "turn-child", Attempt: 1},
			Visit{VisitID: lateDecisionID, StepID: "late-decision", Visit: 1, Iteration: 1,
				Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{ready.VisitID}},
				State:   scheduler.Succeeded, CodexThreadID: "chat-late", TurnID: "turn-late", Attempt: 1,
				Decision: &DecisionRecord{
					Key: "stop", TurnID: "turn-late", CallID: "call-late", To: []string{"limited"},
				}},
		)
		trigger := VisitTrigger{
			Kind: TriggerDecision, SourceVisitIDs: []string{lateDecisionID}, DecisionKey: "stop",
		}
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
			RunFailed, "подделан поздний decision-limit", damaged.Meta.Visits[3].VisitID
		damaged.Meta.StopLimitStepID, damaged.Meta.StopLimitTrigger, damaged.Meta.StopLimitIteration =
			"limited", &trigger, 2
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "раньше заявленной причины") {
			t.Fatalf("поздний limit обошёл причинно доказанный ранний decision-limit: %v", err)
		}
	})

	t.Run("causal descendant proves earlier after limit", func(t *testing.T) {
		initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"earlier-causal-after-limit","start":["dep-a","limit-a","dep-b","limit-b"],"steps":[
    {"id":"dep-a","type":"agent","prompt":"Источник A","after":[]},
    {"id":"limit-a","type":"agent","prompt":"Ранний лимит","after":["dep-a"],"maxVisits":1,"onLimit":"succeeded"},
    {"id":"proof","type":"agent","prompt":"Доказательство завершения A","after":["limit-a"]},
    {"id":"dep-b","type":"agent","prompt":"Источник B","after":[]},
    {"id":"limit-b","type":"agent","prompt":"Поздний лимит","after":["dep-a","dep-b"],"maxVisits":1}
  ]
}`), Task: "Проверить causal proof раннего after-limit", CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		for index := range damaged.Meta.Visits {
			visit := &damaged.Meta.Visits[index]
			visit.State, visit.CodexThreadID, visit.TurnID, visit.Attempt =
				scheduler.Succeeded, fmt.Sprintf("chat-%d", index), fmt.Sprintf("turn-%d", index), 1
		}
		depA, limitA, depB := damaged.Meta.Visits[0], damaged.Meta.Visits[1], damaged.Meta.Visits[2]
		damaged.Meta.Visits = append(damaged.Meta.Visits, Visit{
			VisitID: newID(), StepID: "proof", Visit: 1, Iteration: 1,
			Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{limitA.VisitID}},
			State:   scheduler.Succeeded, CodexThreadID: "chat-proof", TurnID: "turn-proof", Attempt: 1,
		})
		trigger := VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{depA.VisitID, depB.VisitID}}
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
			RunFailed, "подделан поздний after-limit", damaged.Meta.Visits[3].VisitID
		damaged.Meta.StopLimitStepID, damaged.Meta.StopLimitTrigger, damaged.Meta.StopLimitIteration =
			"limit-b", &trigger, 1
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "раньше заявленной причины") {
			t.Fatalf("поздний limit обошёл причинно доказанный ранний after-limit: %v", err)
		}
	})

	t.Run("earlier after limit", func(t *testing.T) {
		initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"after-limit-priority","start":["dep","limit-a","limit-b"],"steps":[
    {"id":"dep","type":"agent","prompt":"Источник","after":[]},
    {"id":"limit-a","type":"agent","prompt":"Первый лимит","after":["dep"],"maxVisits":1,"onLimit":"succeeded"},
    {"id":"limit-b","type":"agent","prompt":"Второй лимит","after":["limit-a","dep"],"maxVisits":1}
  ]
}`), Task: "Проверить after-limit priority", CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		for index := range damaged.Meta.Visits {
			visit := &damaged.Meta.Visits[index]
			visit.State, visit.CodexThreadID, visit.TurnID, visit.Attempt =
				scheduler.Succeeded, fmt.Sprintf("chat-%d", index), fmt.Sprintf("turn-%d", index), 1
		}
		trigger := VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{
			damaged.Meta.Visits[1].VisitID, damaged.Meta.Visits[0].VisitID,
		}}
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
			RunFailed, "подделанный поздний after-limit", damaged.Meta.Visits[2].VisitID
		damaged.Meta.StopLimitStepID, damaged.Meta.StopLimitTrigger, damaged.Meta.StopLimitIteration = "limit-b", &trigger, 1
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "раньше заявленной причины") {
			t.Fatalf("after-limit обошёл более ранний шаг workflow: %v", err)
		}
	})

	t.Run("terminal cleanup in causal ancestry", func(t *testing.T) {
		initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"after-limit-cleanup-ancestry","start":["driver","pending","real","join"],"steps":[
    {"id":"driver","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "main":{"to":["selected"]},"other":{"to":["skipped"]}}},
    {"id":"pending","type":"agent","prompt":"Ожидает","after":[]},
    {"id":"real","type":"agent","prompt":"Реальный источник","after":[]},
    {"id":"join","type":"agent","prompt":"Ограниченный join","after":["mixed","real"],"maxVisits":1},
    {"id":"selected","type":"agent","prompt":"Выбран","after":[]},
    {"id":"skipped","type":"agent","prompt":"Пропущен","after":[]},
    {"id":"mixed","type":"agent","prompt":"Смешанный барьер","after":["pending","skipped"]}
  ]
}`), Task: "Проверить cleanup ancestry", CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		driver := &damaged.Meta.Visits[0]
		driver.State, driver.CodexThreadID, driver.TurnID, driver.Attempt = scheduler.Succeeded, "chat-driver", "turn-driver", 1
		driver.Decision = &DecisionRecord{
			Key: "main", TurnID: "turn-driver", CallID: "call-driver", To: []string{"selected"},
			Skipped: []string{"other"}, Applied: true,
		}
		damaged.Meta.Visits[1].State = scheduler.Skipped
		for index := 2; index <= 3; index++ {
			visit := &damaged.Meta.Visits[index]
			visit.State, visit.CodexThreadID, visit.TurnID, visit.Attempt =
				scheduler.Succeeded, fmt.Sprintf("chat-%d", index), fmt.Sprintf("turn-%d", index), 1
		}
		selectedID, skippedID, mixedID := newID(), newID(), newID()
		damaged.Meta.Visits = append(damaged.Meta.Visits,
			Visit{VisitID: selectedID, StepID: "selected", Visit: 1, Iteration: 2, State: scheduler.Skipped,
				Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{driver.VisitID}, DecisionKey: "main"}},
			Visit{VisitID: skippedID, StepID: "skipped", Visit: 1, Iteration: 2, State: scheduler.Skipped,
				Trigger: VisitTrigger{Kind: TriggerDecisionSkipped, SourceVisitIDs: []string{driver.VisitID}, DecisionKey: "other"}},
			Visit{VisitID: mixedID, StepID: "mixed", Visit: 1, Iteration: 2, State: scheduler.Skipped,
				Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{damaged.Meta.Visits[1].VisitID, skippedID}}},
		)
		trigger := VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{mixedID, damaged.Meta.Visits[2].VisitID}}
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
			RunFailed, "подделанный after-limit из terminal closure", damaged.Meta.Visits[3].VisitID
		damaged.Meta.StopLimitStepID, damaged.Meta.StopLimitTrigger, damaged.Meta.StopLimitIteration = "join", &trigger, 2
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "rootless terminal cleanup") {
			t.Fatalf("after-limit использовал post-terminal causal chain: %v", err)
		}
	})

	t.Run("causal FIFO receipt before proof boundary", func(t *testing.T) {
		limitA, limitB := 1, 2
		workflowSteps := []workflow.Step{
			{ID: "dep"},
			{ID: "z", After: []string{"limit-a"}},
			{ID: "limit-a", After: []string{"dep"}, MaxVisits: &limitA},
			{ID: "limit-b", After: []string{"dep", "z"}, MaxVisits: &limitB},
		}
		steps := make(map[string]workflow.Step, len(workflowSteps))
		for _, step := range workflowSteps {
			steps[step.ID] = step
		}
		history := []Visit{
			{VisitID: "d1", StepID: "dep", Iteration: 1, State: scheduler.Succeeded},
			{VisitID: "z1", StepID: "z", Iteration: 1, State: scheduler.Succeeded},
			{VisitID: "a1", StepID: "limit-a", Iteration: 1, State: scheduler.Succeeded,
				Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{"d1"}}},
			{VisitID: "b1", StepID: "limit-b", Iteration: 1, State: scheduler.Succeeded,
				Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{"d1", "z1"}}},
			{VisitID: "d2", StepID: "dep", Iteration: 2, State: scheduler.Skipped},
			{VisitID: "z2", StepID: "z", Iteration: 2, State: scheduler.Succeeded},
			{VisitID: "a2", StepID: "limit-a", Iteration: 2, State: scheduler.Skipped,
				Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{"d2"}}},
			{VisitID: "b2", StepID: "limit-b", Iteration: 2, State: scheduler.Succeeded,
				Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{"d2", "z2"}}},
			{VisitID: "d3", StepID: "dep", Iteration: 3, State: scheduler.Succeeded},
			// z3 доказывает, что последнее runnable-посещение A уже было
			// terminal до заявленного limit B, но не делает causal a2 своим
			// предком. Его ранний append всё равно доказывает pre-terminal use.
			{VisitID: "z3", StepID: "z", Iteration: 3, State: scheduler.Succeeded,
				Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{"a1"}}},
		}
		seen := make(map[string]Visit, len(history))
		for _, visit := range history {
			seen[visit.VisitID] = visit
		}
		err := validateAfterLimitPriority(
			workflowSteps, history, seen, steps, map[string][]string{"d2": {"root"}, "a2": {"root"}},
			map[string]int{"limit-a": 1, "limit-b": 2},
			map[string]Visit{"limit-a": history[2], "limit-b": history[7]}, map[string]bool{},
			map[string]bool{"a1": true},
			"limit-b", VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{"d3", "z3"}},
		)
		if err == nil || !strings.Contains(err.Error(), "раньше заявленной причины") {
			t.Fatalf("ранняя causal-квитанция скрыла первый after-limit A: %v", err)
		}
	})
}

// TestAgentGraphTerminalRejectsForgedDecisionCleanup защищает decision-wave
// barrier от подмены metadata. Finish и maxVisits не могли быть выбраны, пока
// существовал Pending decision visit; простая замена его состояния на rootless
// Skipped не должна задним числом делать terminal snapshot допустимым.
func TestAgentGraphTerminalRejectsForgedDecisionCleanup(t *testing.T) {
	t.Run("finish", func(t *testing.T) {
		initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"forged-finish-cleanup","start":["blocked","finisher"],"steps":[
    {"id":"blocked","type":"agent","prompt":"Жди","after":[],"decisions":{"go":{"to":["target"]}}},
    {"id":"finisher","type":"agent","prompt":"Заверши","after":[],"decisions":{"done":{"finish":"succeeded"}}},
    {"id":"target","type":"agent","prompt":"Цель","after":[]}
  ]
}`), Task: "Проверить barrier finish", CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		damaged.Meta.Visits[0].State = scheduler.Skipped
		finisher := &damaged.Meta.Visits[1]
		finisher.State, finisher.CodexThreadID, finisher.TurnID, finisher.Attempt =
			scheduler.Succeeded, "chat-finisher", "turn-finisher", 1
		outcome := workflow.OutcomeSucceeded
		finisher.Decision = &DecisionRecord{
			Key: "done", TurnID: "turn-finisher", CallID: "call-finisher", Finish: &outcome, Applied: true,
		}
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
			RunSucceeded, "подделанный finish", finisher.VisitID
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "Pending до terminal cleanup") {
			t.Fatalf("finish скрыл открытую decision-wave: %v", err)
		}
	})

	t.Run("maxVisits", func(t *testing.T) {
		initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"forged-limit-cleanup","start":["blocked","source","limited"],"steps":[
    {"id":"blocked","type":"agent","prompt":"Жди","after":[],"decisions":{"go":{"to":["target"]}}},
    {"id":"source","type":"agent","prompt":"Повтори","after":[],"decisions":{"go":{"to":["limited"]}}},
    {"id":"limited","type":"agent","prompt":"Лимит","after":[],"maxVisits":1},
    {"id":"target","type":"agent","prompt":"Цель","after":[]}
  ]
}`), Task: "Проверить barrier maxVisits", CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, initial)
		damaged.Meta.Visits[0].State = scheduler.Skipped
		source := &damaged.Meta.Visits[1]
		source.State, source.CodexThreadID, source.TurnID, source.Attempt =
			scheduler.Succeeded, "chat-source", "turn-source", 1
		source.Decision = &DecisionRecord{
			Key: "go", TurnID: "turn-source", CallID: "call-source", To: []string{"limited"},
		}
		limited := &damaged.Meta.Visits[2]
		limited.State, limited.CodexThreadID, limited.TurnID, limited.Attempt =
			scheduler.Succeeded, "chat-limited", "turn-limited", 1
		trigger := VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{source.VisitID}, DecisionKey: "go"}
		damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
			RunFailed, "подделанный maxVisits", limited.VisitID
		damaged.Meta.StopLimitStepID, damaged.Meta.StopLimitTrigger, damaged.Meta.StopLimitIteration = "limited", &trigger, 2
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "decision visit") {
			t.Fatalf("maxVisits скрыл открытую decision-wave: %v", err)
		}
	})
}

// TestAgentGraphTerminalCleanupReplaysFIFO не позволяет более ранней
// terminal-квитанции обойти real source, если поздняя запись утверждает, что
// этот source уже породил Pending до outcome. Весь префикс до cleanup обязан
// быть достижим по обычным running FIFO-правилам без terminal bypass.
func TestAgentGraphTerminalCleanupReplaysFIFO(t *testing.T) {
	initial, err := Create(t.TempDir(), Input{WorkflowJSON: []byte(`{
  "version":2,"id":"cleanup-fifo-prefix","start":["driver","source"],"steps":[
    {"id":"driver","type":"agent","prompt":"Выбрать ветку","after":[],"decisions":{
      "main":{"to":["selected"]},"other":{"to":["source"]}}},
    {"id":"source","type":"agent","prompt":"FIFO источник","after":[]},
    {"id":"selected","type":"agent","prompt":"Выбранная ветка","after":[]},
    {"id":"receipt","type":"agent","prompt":"FIFO квитанция","after":["source"]},
    {"id":"fatal","type":"agent","prompt":"Fatal решение","after":["selected"],"decisions":{"go":{"to":["sink"]}}},
    {"id":"sink","type":"agent","prompt":"Не запускается","after":[]}
  ]
}`), Task: "Проверить pre-terminal FIFO cleanup", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	damaged := cloneSnapshotMetadata(t, initial)
	driver := &damaged.Meta.Visits[0]
	driver.State, driver.CodexThreadID, driver.TurnID, driver.Attempt =
		scheduler.Succeeded, "chat-driver", "turn-driver", 1
	driver.Decision = &DecisionRecord{
		Key: "main", TurnID: "turn-driver", CallID: "call-driver", To: []string{"selected"},
		Skipped: []string{"other"}, Applied: true,
	}
	source := &damaged.Meta.Visits[1]
	source.State, source.CodexThreadID, source.TurnID, source.Attempt =
		scheduler.Succeeded, "chat-source", "turn-source", 1
	selectedID, skippedSourceID, fatalID := newID(), newID(), newID()
	damaged.Meta.Visits = append(damaged.Meta.Visits,
		Visit{VisitID: selectedID, StepID: "selected", Visit: 1, Iteration: 2,
			Trigger: VisitTrigger{Kind: TriggerDecision, SourceVisitIDs: []string{driver.VisitID}, DecisionKey: "main"},
			State:   scheduler.Succeeded, CodexThreadID: "chat-selected", TurnID: "turn-selected", Attempt: 1},
		Visit{VisitID: skippedSourceID, StepID: "source", Visit: 2, Iteration: 2, State: scheduler.Skipped,
			Trigger: VisitTrigger{Kind: TriggerDecisionSkipped, SourceVisitIDs: []string{driver.VisitID}, DecisionKey: "other"}},
		Visit{VisitID: newID(), StepID: "receipt", Visit: 1, Iteration: 2, State: scheduler.Skipped,
			Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{skippedSourceID}}},
		Visit{VisitID: newID(), StepID: "receipt", Visit: 2, Iteration: 1, State: scheduler.Skipped,
			Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{source.VisitID}}},
		Visit{VisitID: fatalID, StepID: "fatal", Visit: 1, Iteration: 2,
			Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{selectedID}},
			State:   scheduler.Succeeded, CodexThreadID: "chat-fatal", TurnID: "turn-fatal", Attempt: 1,
			Decision: &DecisionRecord{
				Key: "go", TurnID: "turn-fatal", CallID: "call-fatal", To: []string{"sink"},
				Error: "конфликт durable decision",
			}},
	)
	damaged.Meta.RunState, damaged.Meta.StopReason, damaged.Meta.StopVisitID =
		RunFailed, "fatal после невозможного FIFO", fatalID
	if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "pre-terminal history") {
		t.Fatalf("terminal FIFO принял невозможный append-order вокруг cleanup: %v", err)
	}
}

// TestAgentGraphTerminalRequiresCompleteSkippedClosure проверяет обратную
// сторону terminal materialization. Присутствие Skipped означает новый формат
// поведения: из него нельзя удалить последний причинный слой или вернуть один
// cleanup visit в Pending, потому что после outcome обычный Advance запрещён и
// такая ветка навсегда исчезла бы из статуса и dashboard.
func TestAgentGraphTerminalRequiresCompleteSkippedClosure(t *testing.T) {
	t.Run("missing final causal layer", func(t *testing.T) {
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
		finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "safe")
		finished, err := run.AdvanceAgentGraph()
		if err != nil {
			t.Fatal(err)
		}
		last := finished.Snapshot.Meta.Visits[len(finished.Snapshot.Meta.Visits)-1]
		if last.StepID != "summary" || last.State != scheduler.Skipped {
			t.Fatalf("fixture не завершилась ожидаемым closure-слоем: %+v", last)
		}
		damaged := cloneSnapshotMetadata(t, finished.Snapshot)
		damaged.Meta.Visits = damaged.Meta.Visits[:len(damaged.Meta.Visits)-1]
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "неполное skipped-замыкание") {
			t.Fatalf("terminal snapshot принял удалённый causal layer: %v", err)
		}
	})

	t.Run("pending left after terminal", func(t *testing.T) {
		root := t.TempDir()
		initial, err := Create(root, advanceInput(t))
		if err != nil {
			t.Fatal(err)
		}
		run, err := OpenLocked(root, initial.Meta.RunID)
		if err != nil {
			t.Fatal(err)
		}
		defer run.Close()
		finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "done")
		finished, err := run.AdvanceAgentGraph()
		if err != nil {
			t.Fatal(err)
		}
		damaged := cloneSnapshotMetadata(t, finished.Snapshot)
		damaged.Meta.Visits[1].State = scheduler.Pending
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "оставил Pending") {
			t.Fatalf("новый terminal snapshot принял незакрытый Pending: %v", err)
		}
	})
}

// TestAgentGraphNestedSkippedValidation защищает восстановимую causal wave.
// Вложенный target обязан ссылаться на пропущенный decision, использовать его
// канонический ключ и не посещать общий step второй раз в рамках тех же roots.
// Эти проверки не доверяют planner: повреждённый meta.json отклоняется при Load.
func TestAgentGraphNestedSkippedValidation(t *testing.T) {
	t.Run("key and source", func(t *testing.T) {
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
		finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "safe")
		advanced, err := run.AdvanceAgentGraph()
		if err != nil {
			t.Fatal(err)
		}
		leafIndex, summaryIndex := -1, -1
		for index, visit := range advanced.Snapshot.Meta.Visits {
			if visit.StepID == "leaf-a" {
				leafIndex = index
			}
			if visit.StepID == "summary" {
				summaryIndex = index
			}
		}
		if leafIndex < 0 || summaryIndex < 0 {
			t.Fatalf("fixture не создала вложенную ветку: %+v", advanced.Snapshot.Meta.Visits)
		}
		for name, mutate := range map[string]func(*Snapshot){
			"wrong nested key": func(snapshot *Snapshot) {
				snapshot.Meta.Visits[leafIndex].Trigger.DecisionKey = "b"
			},
			"wrong source": func(snapshot *Snapshot) {
				snapshot.Meta.Visits[leafIndex].Trigger.SourceVisitIDs = []string{snapshot.Meta.Visits[0].VisitID}
			},
			"completed all-skipped after": func(snapshot *Snapshot) {
				visit := &snapshot.Meta.Visits[summaryIndex]
				visit.State, visit.CodexThreadID, visit.TurnID, visit.Attempt =
					scheduler.Succeeded, "chat-forged-summary", "turn-forged-summary", 1
			},
		} {
			t.Run(name, func(t *testing.T) {
				damaged := cloneSnapshotMetadata(t, advanced.Snapshot)
				mutate(&damaged)
				if err := damaged.validate(damaged.Meta.RunID); err == nil {
					t.Fatal("повреждённая nested skipped-причина принята")
				}
			})
		}
	})

	t.Run("same wave duplicate", func(t *testing.T) {
		root := t.TempDir()
		input := Input{WorkflowJSON: []byte(`{
  "version":2,"id":"shared-skip-wave","start":["choice"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "nested":{"to":["left","right"]},"safe":{"finish":"succeeded"}}},
    {"id":"left","type":"agent","prompt":"Левая","after":[],"decisions":{"go":{"to":["shared"]}}},
    {"id":"right","type":"agent","prompt":"Правая","after":[],"decisions":{"go":{"to":["shared"]}}},
    {"id":"shared","type":"agent","prompt":"Общая","after":[]}
  ]
}`), Task: "Проверить shared target одной волны", CWD: t.TempDir()}
		initial, err := Create(root, input)
		if err != nil {
			t.Fatal(err)
		}
		run, err := OpenLocked(root, initial.Meta.RunID)
		if err != nil {
			t.Fatal(err)
		}
		defer run.Close()
		finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "safe")
		advanced, err := run.AdvanceAgentGraph()
		if err != nil {
			t.Fatal(err)
		}
		var right Visit
		for _, visit := range advanced.Snapshot.Meta.Visits {
			if visit.StepID == "right" {
				right = visit
			}
		}
		if right.VisitID == "" {
			t.Fatalf("fixture не создала правый skipped decision: %+v", advanced.Snapshot.Meta.Visits)
		}
		damaged := cloneSnapshotMetadata(t, advanced.Snapshot)
		damaged.Meta.Visits = append(damaged.Meta.Visits, Visit{
			VisitID: newID(), StepID: "shared", Visit: 2, Iteration: right.Iteration + 1, State: scheduler.Skipped,
			Trigger: VisitTrigger{Kind: TriggerDecisionSkipped, SourceVisitIDs: []string{right.VisitID}, DecisionKey: "go"},
		})
		if err := damaged.validate(damaged.Meta.RunID); err == nil || !strings.Contains(err.Error(), "волна пропуска") {
			t.Fatalf("общий target той же skip-wave принят повторно: %v", err)
		}
	})
}

// TestAfterTriggerFIFO не позволяет переставить причинность уже завершённых
// посещений: каждый after-barrier обязан потреблять их в durable-порядке Visits.
func TestAfterTriggerFIFO(t *testing.T) {
	first := Visit{VisitID: newID(), StepID: "work", State: scheduler.Succeeded, Iteration: 1}
	second := Visit{VisitID: newID(), StepID: "work", State: scheduler.Succeeded, Iteration: 2}
	history := []Visit{first, second}
	seen := map[string]Visit{first.VisitID: first, second.VisitID: second}
	uses := map[afterCause]bool{}
	trigger := func(source Visit) error {
		return validateTrigger(2, Visit{StepID: "check", Iteration: source.Iteration,
			Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{source.VisitID}}},
			workflow.Step{ID: "check", After: []string{"work"}}, nil, history, seen, nil, uses, map[decisionCause]bool{})
	}
	if err := trigger(second); err == nil {
		t.Fatal("after принял более поздний источник раньше первого")
	}
	if err := trigger(first); err != nil {
		t.Fatalf("after отклонил самый ранний источник: %v", err)
	}
	if err := trigger(second); err != nil {
		t.Fatalf("after не перешёл к следующему источнику: %v", err)
	}

	// Незавершённый ранний instance остаётся первым в FIFO. Более поздний
	// synthetic Skipped нельзя использовать как обход блокирующего Running.
	running := Visit{VisitID: newID(), StepID: "work", State: scheduler.Running, Iteration: 1, CodexThreadID: "chat", TurnID: "turn", Attempt: 1}
	skipped := Visit{VisitID: newID(), StepID: "work", State: scheduler.Skipped, Iteration: 2}
	history = []Visit{running, skipped}
	seen = map[string]Visit{running.VisitID: running, skipped.VisitID: skipped}
	_, err := validateTriggerWithSkipWaves(2, Visit{
		StepID: "check", Iteration: 2, State: scheduler.Skipped,
		Trigger: VisitTrigger{Kind: TriggerAfter, SourceVisitIDs: []string{skipped.VisitID}},
	}, workflow.Step{ID: "check", After: []string{"work"}}, nil, history, seen, nil,
		map[afterCause]bool{}, map[decisionCause]bool{}, map[string][]string{skipped.VisitID: {newID()}}, map[skipReach]bool{}, false, "")
	if err == nil {
		t.Fatal("after обошёл ранний Running через более поздний Skipped")
	}
}

func cloneSnapshotMetadata(t *testing.T, snapshot Snapshot) Snapshot {
	t.Helper()
	data, err := json.Marshal(snapshot.Meta)
	if err != nil {
		t.Fatal(err)
	}
	result := Snapshot{Workflow: snapshot.Workflow, Task: snapshot.Task, HistoricalAppNative: snapshot.HistoricalAppNative}
	if err := json.Unmarshal(data, &result.Meta, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	return result
}
