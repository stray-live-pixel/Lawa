//go:build darwin || linux

package runstore

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
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

func testAdvanceRun(t *testing.T) (string, Snapshot, *LockedRun) {
	t.Helper()
	root := t.TempDir()
	snapshot, err := CreateAgentGraph(root, advanceInput(t))
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
	if err := run.ReserveVisits([]string{visitID}); err == nil {
		err = run.UpdateVisit(visitID, scheduler.Unknown, "chat-decision", "")
		if err == nil {
			err = run.SetVisitTurn(visitID, "turn-decision")
		}
		if err == nil {
			_, err = run.CommitDecision(visitID, "chat-decision", "turn-decision", key, "выбран маршрут", "call-decision")
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
		!result.Snapshot.Meta.Visits[0].Decision.Applied || result.Snapshot.Meta.Visits[1].State != scheduler.Running || len(result.CreatedVisits) != 0 {
		t.Fatalf("finish опубликован неатомарно: %+v", result)
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
	initial, err := CreateAgentGraph(root, input)
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

// TestAdvanceAgentGraphGuards отклоняет legacy, циклическую и повреждённую
// историю до любой новой памяти или metadata.
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
	t.Run("cycle", func(t *testing.T) {
		input := advanceInput(t)
		input.WorkflowJSON = []byte(`{"version":2,"id":"cycle","start":["loop"],"steps":[
          {"id":"loop","type":"agent","prompt":"Повтори","after":[],"maxVisits":2,
           "decisions":{"repeat":{"to":["loop"]},"done":{"finish":"succeeded"}}}]}`)
		root := t.TempDir()
		snapshot, err := CreateAgentGraph(root, input)
		if err != nil {
			t.Fatal(err)
		}
		run, err := OpenLocked(root, snapshot.Meta.RunID)
		if err != nil {
			t.Fatal(err)
		}
		defer run.Close()
		if _, err := run.AdvanceAgentGraph(); !errors.Is(err, scheduler.ErrAgentGraphCycle) {
			t.Fatalf("циклический workflow прошёл первый runtime: %v", err)
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
