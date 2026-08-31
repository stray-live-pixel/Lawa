package coordinator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// TestApplyRuntimeSettingsModelPriority фиксирует все три ступени выбора модели:
// значение кубика, общий default workflow и наследование Codex через пустую Command.
func TestApplyRuntimeSettingsModelPriority(t *testing.T) {
	workflowModel, stepModel := "gpt-5.6-luna", "gpt-5.6-sol"
	for _, tc := range []struct {
		name          string
		workflowModel *string
		stepModel     *string
		want          string
	}{
		{name: "step", workflowModel: &workflowModel, stepModel: &stepModel, want: stepModel},
		{name: "workflow", workflowModel: &workflowModel, want: workflowModel},
		{name: "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var command codex.Command
			applyRuntimeSettings(&command, tc.workflowModel, workflow.Step{Model: tc.stepModel})
			if command.Model != tc.want {
				t.Fatalf("неверная модель для уровня %s: получено %q, ожидалось %q", tc.name, command.Model, tc.want)
			}
		})
	}
}

// TestPrepare резервирует независимые шаги одной волной и проверяет, что
// повторное планирование не возвращает их второй раз. Сетевых запросов в тесте
// нет: Starting обязан быть сохранён ещё до передачи Command вызывающему коду.
func TestPrepare(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(`{"id":"demo","model":"gpt-5.6-luna","steps":[{"id":"first","type":"agent","prompt":"Первая задача","dependsOn":[],"model":"gpt-5.6-sol","effort":"high","speed":"fast"},{"id":"second","type":"agent","prompt":"Вторая задача","dependsOn":[],"speed":"normal"}]}`),
		Task:         "Сделать MVP", Comment: "Проверить границы", CWD: cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	prepared, err := Prepare(run, root)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Complete || len(prepared.Waiting) != 0 || len(prepared.Launches) != 2 {
		t.Fatalf("неверная подготовка: %+v", prepared)
	}
	for index, launch := range prepared.Launches {
		wantID := []string{"first", "second"}[index]
		wantTitle := "Lawa: demo / " + wantID + " [" + snapshot.Meta.RunID + "]"
		if launch.StepID != wantID || launch.Command.CWD != cwd || launch.Command.Title != wantTitle {
			t.Fatalf("неверная команда %d: %+v", index, launch)
		}
		if wantID == "first" && (launch.Command.Model != "gpt-5.6-sol" || launch.Command.Effort != "high" || launch.Command.ServiceTier != "fast") {
			t.Fatalf("первый кубик потерял явные настройки Codex: %+v", launch.Command)
		}
		if wantID == "second" && (launch.Command.Model != "gpt-5.6-luna" || launch.Command.Effort != "" || launch.Command.ServiceTier != "default") {
			t.Fatalf("общая модель, normal или наследуемые настройки второго кубика искажены: %+v", launch.Command)
		}
		profile := launch.Command.Permissions
		wantRunDir := filepath.Join(root, snapshot.Meta.RunID)
		wantMemory := filepath.Join(wantRunDir, "memory", snapshot.Meta.Steps[index].ThreadID+".md")
		if profile == nil || profile.Name != "lawa-"+snapshot.Meta.Steps[index].ThreadID ||
			len(profile.ReadPaths) != 1 || profile.ReadPaths[0] != wantRunDir ||
			len(profile.WritePaths) != 1 || profile.WritePaths[0] != wantMemory {
			t.Fatalf("шаг %q получил неверные права памяти: %+v", wantID, profile)
		}
		for _, fragment := range []string{snapshot.Meta.RunID, snapshot.Meta.Steps[index].ThreadID, "Сделать MVP", "Проверить границы", "Первая задача", "Вторая задача", "memory/", "обновляй только этот файл"} {
			// В команде конкретного шага должен быть только его prompt, поэтому
			// проверяем подходящий фрагмент задачи отдельно от общего набора.
			if (fragment == "Первая задача" && wantID != "first") || (fragment == "Вторая задача" && wantID != "second") {
				continue
			}
			if !strings.Contains(launch.Command.Text, fragment) {
				t.Errorf("команда %q не содержит %q", wantID, fragment)
			}
		}
	}

	saved, err := run.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range saved.Meta.Steps {
		if step.State != scheduler.Starting || step.CodexThreadID != "" {
			t.Errorf("шаг не зарезервирован до сети: %+v", step)
		}
	}
	again, err := Prepare(run, root)
	if err != nil || len(again.Launches) != 0 || again.Complete {
		t.Fatalf("повтор разрешил дубликат: %+v, %v", again, err)
	}
}

// TestPrepareContinuationKeepsRuntimeSettings подтверждает контракт resume:
// настройки берутся из неизменяемого workflow.json исходного run, а не из
// текущего конфига Codex или последнего процесса Lawa.
func TestPrepareContinuationKeepsRuntimeSettings(t *testing.T) {
	root := t.TempDir()
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(`{"id":"resume","model":"gpt-5.6-terra","steps":[{"id":"step","type":"agent","prompt":"Задача","dependsOn":[],"effort":"medium","speed":"fast"}]}`),
		Task:         "Задача", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	if err := run.Reserve([]string{"step"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Update("step", scheduler.Cancelled, "existing-chat"); err != nil {
		t.Fatal(err)
	}
	saved, err := run.Load()
	if err != nil {
		t.Fatal(err)
	}
	continuations, err := prepareContinuations(saved, root, true, map[string]bool{})
	if err != nil || len(continuations) != 1 {
		t.Fatalf("продолжение не подготовлено: %+v, %v", continuations, err)
	}
	command := continuations[0].Command
	if command.Model != "gpt-5.6-terra" || command.Effort != "medium" || command.ServiceTier != "fast" || command.Text != "continue" {
		t.Fatalf("resume изменил настройки кубика: %+v", command)
	}
}

// TestPrepareWaitsForDependencies проверяет границу между планировщиком и
// координатором: зависимый шаг не резервируется до подтверждённого успеха.
func TestPrepareWaitsForDependencies(t *testing.T) {
	root := t.TempDir()
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(`{"id":"chain","steps":[{"id":"child","type":"agent","prompt":"Итог","dependsOn":["parent"]},{"id":"parent","type":"agent","prompt":"Факты","dependsOn":[]}]}`),
		Task:         "Задача", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	prepared, err := Prepare(run, root)
	if err != nil || len(prepared.Launches) != 1 || prepared.Launches[0].StepID != "parent" || len(prepared.Waiting) != 1 || prepared.Waiting[0] != "child" {
		t.Fatalf("зависимость нарушена: %+v, %v", prepared, err)
	}
}

// TestPrepareRejectsNilRun не допускает panic на ошибке связывания CLI.
func TestPrepareRejectsNilRun(t *testing.T) {
	if _, err := Prepare(nil, filepath.Join(string(os.PathSeparator), "tmp")); err == nil {
		t.Fatal("nil-запуск принят")
	}
}

// TestPrepareChecksRoot проверяет root до необратимого Pending → Starting.
// Ошибка пути не должна резервировать шаг, потому что агент не сможет вести память.
func TestPrepareChecksRoot(t *testing.T) {
	root := t.TempDir()
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(`{"id":"one","steps":[{"id":"step","type":"agent","prompt":"Задача","dependsOn":[]}]}`),
		Task:         "Задача", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	if _, err := Prepare(run, ""); err == nil {
		t.Fatal("пустой root принят")
	}
	if _, err := Prepare(run, t.TempDir()); err == nil {
		t.Fatal("чужой root принят")
	}
	saved, err := run.Load()
	if err != nil || saved.Meta.Steps[0].State != scheduler.Pending {
		t.Fatalf("ошибка root зарезервировала шаг: %+v, %v", saved.Meta.Steps, err)
	}
}
