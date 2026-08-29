package coordinator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/flows-2/internal/runstore"
	"github.com/stray-live-pixel/flows-2/internal/scheduler"
)

// TestPrepare резервирует независимые шаги одной волной и проверяет, что
// повторное планирование не возвращает их второй раз. Сетевых запросов в тесте
// нет: Starting обязан быть сохранён ещё до передачи Command вызывающему коду.
func TestPrepare(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(`{"id":"demo","steps":[{"id":"first","type":"agent","prompt":"Первая задача","dependsOn":[]},{"id":"second","type":"agent","prompt":"Вторая задача","dependsOn":[]}]}`),
		Task:         "Сделать MVP", Comment: "Проверить границы", CWD: cwd, InitiatorThreadID: "initiator",
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
		if launch.StepID != wantID || launch.Command.CWD != cwd || launch.Command.Title != "Lawa: demo / "+wantID {
			t.Fatalf("неверная команда %d: %+v", index, launch)
		}
		for _, fragment := range []string{"Сделать MVP", "Проверить границы", "Первая задача", "Вторая задача", "memory/", "обновляй только этот файл"} {
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

// TestPrepareWaitsForDependencies проверяет границу между планировщиком и
// координатором: зависимый шаг не резервируется до подтверждённого успеха.
func TestPrepareWaitsForDependencies(t *testing.T) {
	root := t.TempDir()
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(`{"id":"chain","steps":[{"id":"child","type":"agent","prompt":"Итог","dependsOn":["parent"]},{"id":"parent","type":"agent","prompt":"Факты","dependsOn":[]}]}`),
		Task:         "Задача", CWD: t.TempDir(), InitiatorThreadID: "initiator",
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
		Task:         "Задача", CWD: t.TempDir(), InitiatorThreadID: "initiator",
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
