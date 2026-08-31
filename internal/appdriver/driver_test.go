package appdriver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

func TestAppDriverParallelWorkflowAndRecovery(t *testing.T) {
	root, snapshot := createTestRun(t, `{
  "id":"native",
  "steps":[
    {"id":"first","type":"agent","prompt":"first task","dependsOn":[]},
    {"id":"second","type":"agent","prompt":"second task","dependsOn":[]},
    {"id":"final","type":"agent","prompt":"combine","dependsOn":["first","second"]}
  ]
}`)

	first := next(t, root, snapshot.Meta.RunID)
	if first.Kind != "launch" || first.Launch.StepID != "first" || first.Launch.CodexThreadID != "" {
		t.Fatalf("первый app-next = %#v", first)
	}
	// Повтор до create_thread обязан вернуть тот же зарезервированный кубик. Если
	// сразу выдать second, потеря ответа create могла бы оставить дубликат first.
	repeated := next(t, root, snapshot.Meta.RunID)
	if repeated.Launch == nil || repeated.Launch.StepID != "first" || repeated.Launch.Title != first.Launch.Title {
		t.Fatalf("повтор app-next = %#v", repeated)
	}

	bind(t, root, snapshot.Meta.RunID, "first", "codex-first")
	recovery := next(t, root, snapshot.Meta.RunID)
	if recovery.Launch == nil || recovery.Launch.CodexThreadID != "codex-first" || recovery.Launch.Prompt != first.Launch.Prompt {
		t.Fatalf("восстановление после bind = %#v", recovery)
	}
	update(t, root, snapshot.Meta.RunID, "first", "running", nil)

	// После надёжной привязки first независимый second запускается до завершения
	// first. Именно так app-native режим сохраняет параллельность workflow.
	second := next(t, root, snapshot.Meta.RunID)
	if second.Launch == nil || second.Launch.StepID != "second" {
		t.Fatalf("параллельный app-next = %#v", second)
	}
	bind(t, root, snapshot.Meta.RunID, "second", "codex-second")
	update(t, root, snapshot.Meta.RunID, "second", "running", nil)
	update(t, root, snapshot.Meta.RunID, "second", "succeeded", []byte("second result"))

	observed := next(t, root, snapshot.Meta.RunID)
	if observed.Kind != "observe" || len(observed.Tasks) != 1 || observed.Tasks[0].StepID != "first" ||
		len(observed.Waiting) != 1 || observed.Waiting[0] != "final" {
		t.Fatalf("наблюдение незавершённой волны = %#v", observed)
	}
	update(t, root, snapshot.Meta.RunID, "first", "succeeded", []byte("first result"))

	final := next(t, root, snapshot.Meta.RunID)
	if final.Launch == nil || final.Launch.StepID != "final" {
		t.Fatalf("зависимый кубик = %#v", final)
	}
	if got := readStepMemory(t, root, snapshot.Meta.RunID, "first"); got != "first result" {
		t.Fatalf("память first = %q", got)
	}
	if got := readStepMemory(t, root, snapshot.Meta.RunID, "second"); got != "second result" {
		t.Fatalf("память second = %q", got)
	}
	bind(t, root, snapshot.Meta.RunID, "final", "codex-final")
	update(t, root, snapshot.Meta.RunID, "final", "succeeded", []byte("all done"))
	if done := next(t, root, snapshot.Meta.RunID); done.Kind != "complete" {
		t.Fatalf("финальный app-next = %#v", done)
	}
}

func TestAppDriverKeepsFailedTaskIdentityForManualContinuation(t *testing.T) {
	root, snapshot := createTestRun(t, `{
  "id":"retry",
  "steps":[
    {"id":"work","type":"agent","prompt":"work","dependsOn":[]},
    {"id":"after","type":"agent","prompt":"after","dependsOn":["work"]}
  ]
}`)
	action := next(t, root, snapshot.Meta.RunID)
	bind(t, root, snapshot.Meta.RunID, action.Launch.StepID, "same-task")
	update(t, root, snapshot.Meta.RunID, "work", "failed", []byte("failure details"))

	failed := next(t, root, snapshot.Meta.RunID)
	if failed.Kind != "observe" || len(failed.Tasks) != 1 || failed.Tasks[0].CodexThreadID != "same-task" ||
		failed.Tasks[0].State != scheduler.Failed {
		t.Fatalf("failed должен остаться наблюдаемой задачей: %#v", failed)
	}
	// Пользователь продолжает эту же задачу в Codex App. Lawa меняет только её
	// статус и после успеха разрешает зависимость, не создавая новый task.
	update(t, root, snapshot.Meta.RunID, "work", "running", nil)
	update(t, root, snapshot.Meta.RunID, "work", "succeeded", []byte("recovered"))
	if after := next(t, root, snapshot.Meta.RunID); after.Launch == nil || after.Launch.StepID != "after" {
		t.Fatalf("после ручного продолжения = %#v", after)
	}
}

func TestAppDriverRejectsDuplicateTaskAndSuccessWithoutResult(t *testing.T) {
	root, snapshot := createTestRun(t, `{
  "id":"single",
  "steps":[{"id":"only","type":"agent","prompt":"work","dependsOn":[]}]
}`)
	_ = next(t, root, snapshot.Meta.RunID)
	bind(t, root, snapshot.Meta.RunID, "only", "original")
	if err := Bind(root, snapshot.Meta.RunID, "only", "duplicate"); err == nil {
		t.Fatal("другой task id должен быть запрещён")
	}
	if err := Update(root, snapshot.Meta.RunID, "only", "succeeded", nil); err == nil {
		t.Fatal("успех без финального ответа должен быть запрещён")
	}
}

func createTestRun(t *testing.T, definition string) (string, runstore.Snapshot) {
	t.Helper()
	root, cwd := t.TempDir(), t.TempDir()
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(definition), Task: "user task", CWD: cwd, InitiatorThreadID: "initiator",
	})
	if err != nil {
		t.Fatal(err)
	}
	return root, snapshot
}

func next(t *testing.T, root, runID string) Action {
	t.Helper()
	action, err := Next(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	return action
}

func bind(t *testing.T, root, runID, stepID, threadID string) {
	t.Helper()
	if err := Bind(root, runID, stepID, threadID); err != nil {
		t.Fatal(err)
	}
}

func update(t *testing.T, root, runID, stepID, state string, result []byte) {
	t.Helper()
	if err := Update(root, runID, stepID, state, result); err != nil {
		t.Fatal(err)
	}
}

func readStepMemory(t *testing.T, root, runID, stepID string) string {
	t.Helper()
	snapshot, err := runstore.Load(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range snapshot.Meta.Steps {
		if step.ID != stepID {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(root, runID, "memory", step.ThreadID+".md"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(data)
	}
	t.Fatalf("нет шага %q", stepID)
	return ""
}
