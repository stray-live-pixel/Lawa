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
