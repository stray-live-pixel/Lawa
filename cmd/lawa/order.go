package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/stray-live-pixel/Lawa/internal/capacity"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/coordinator"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// orderDefinition читает профиль из обычного workflow либо документа только с
// characters. Во втором случае временный кубик нужен общему строгому валидатору,
// но не публикуется как доступный процесс. Старый run по-прежнему требует steps.
func orderDefinition(source string) (workflow.Workflow, []byte, error) {
	definition := workflow.Workflow{ID: "order", Characters: map[string]workflow.Character{"boss": workflow.Boss()}}
	var assigned []byte
	if source != "" {
		path, err := filepath.Abs(source)
		if err != nil {
			return definition, nil, err
		}
		data, err := readWorkflowFile(path)
		if err != nil {
			return definition, nil, err
		}
		var document map[string]jsontext.Value
		if err = json.Unmarshal(data, &document); err != nil {
			return definition, nil, err
		}
		if document == nil {
			return definition, nil, errors.New("описание заказа должно быть JSON-объектом")
		}
		_, hasSteps := document["steps"]
		if !hasSteps {
			document["steps"] = jsontext.Value(`[{"id":"boss","type":"agent","character":"boss","prompt":"Получить заказ Чела","dependsOn":[]}]`)
			data, err = json.Marshal(document)
			if err != nil {
				return definition, nil, err
			}
		}
		assigned, definition, err = workflow.ResolveSource(data, path, readWorkflowFile)
		if err != nil {
			return definition, nil, err
		}
		if !hasSteps {
			assigned = nil
		}
	}
	boss, exists := definition.Characters["boss"]
	if !exists {
		boss = workflow.Boss()
	}
	prompt := workflow.BossAssignment
	if len(assigned) != 0 {
		prompt += "\nДоступный процесс сохранён в " + runstore.AssignedWorkflowFilename + " в папке текущего run (родитель каталога memory из переданного пути памяти). Используй именно этот снимок при запуске дочернего workflow."
	}
	return workflow.Workflow{ID: definition.ID + "-boss", Model: definition.Model,
		Characters: map[string]workflow.Character{"boss": boss},
		Steps:      []workflow.Step{{ID: "boss", Type: "agent", Character: "boss", Prompt: prompt, DependsOn: []string{}}},
	}, assigned, nil
}

// orderCommand — мост из интерактивного Codex-чата: order создаёт нового Босса,
// reply доставляет дословную реплику тому же экземпляру. Оба используют обычное
// хранилище, права и дочерние workflow, не создавая второго runtime или daemon.
func orderCommand(ctx context.Context, args []string, out, stderr io.Writer, deps dependencies, reply bool) (err error) {
	positionals, values, err := parseOptions(args, map[string]bool{
		"cwd": true, "task": true, "task-file": true, "root": true, "codex": true, "max-parallel": true,
	})
	if err != nil {
		return err
	}
	if len(positionals) > 1 || reply && len(positionals) != 1 {
		return errors.New("order принимает необязательный workflow.json; reply требует run-id")
	}
	root, err := resolveRoot(values["root"], deps.userHomeDir)
	if err != nil {
		return err
	}
	var locked *runstore.LockedRun
	var snapshot runstore.Snapshot
	if reply {
		if _, exists := values["cwd"]; exists {
			return errors.New("reply использует сохранённый cwd; не передавайте --cwd")
		}
		locked, err = runstore.OpenLocked(root, positionals[0])
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, locked.Close()) }()
		snapshot, err = locked.Load()
		if err != nil {
			return err
		}
		if snapshot.Meta.Order == nil {
			return errors.New("reply принимает только заказ lawa order")
		}
		values["cwd"] = snapshot.Meta.CWD
	}
	// Повторно используем проверки взаимоисключающих task/task-file и capacity.
	normalized := []string{"order"}
	for name, value := range values {
		normalized = append(normalized, "--"+name+"="+value)
	}
	parsed, err := parseRunArguments(normalized)
	if err != nil {
		return err
	}
	if parsed.taskFile != "" {
		parsed.task, err = readTextArgument(parsed.taskFile, "заказ Чела")
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(parsed.task) == "" {
		return errors.New("нужен непустой заказ или ответ Чела")
	}
	cwd, err := filepath.Abs(parsed.cwd)
	if err != nil {
		return err
	}
	var input runstore.Input
	if !reply {
		source := ""
		if len(positionals) == 1 {
			source = positionals[0]
		}
		definition, assigned, err := orderDefinition(source)
		if err != nil {
			return err
		}
		data, err := json.Marshal(definition)
		if err != nil {
			return err
		}
		input = runstore.Input{Order: true, WorkflowJSON: data, AssignedWorkflowJSON: assigned, Task: parsed.task, CWD: cwd}
	}
	if err = deps.check(ctx, codex.Connection{Executable: parsed.executable, CWD: cwd, Stderr: stderr}); err != nil {
		return err
	}
	pool, err := capacity.Configure(root, parsed.maxParallel)
	if err != nil {
		return err
	}
	// Capacity уже создала root. Канонический путь нужен и в prompt Босса,
	// и в child API: снимок должен открываться даже через --root с симлинком.
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if !reply {
		snapshot, err = runstore.Create(root, input)
		if err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintf(out, "runId: %s\n", snapshot.Meta.RunID); err != nil {
		return err
	}
	if reply {
		manager := newChildRunManager(ctx, root, parsed.executable, pool, stderr, deps)
		err = coordinator.Message(ctx, locked, coordinator.Options{Root: root, Client: deps.client(parsed.executable, stderr, nil), Capacity: pool, ConfigureCommand: manager.configure}, parsed.task)
		err = errors.Join(err, manager.wait())
	} else {
		_, err = coordinateWithOutcome(ctx, root, snapshot.Meta.RunID, parsed.executable, pool, io.Discard, stderr, deps, false, true)
	}
	if err != nil {
		return err
	}
	finished, err := runstore.Load(root, snapshot.Meta.RunID)
	if err != nil {
		return err
	}
	result := finished.Meta.Steps[0].Result
	if result == "" {
		result = "Босс завершил ход без отчёта «Итог:»; прочитайте lawa logs этого run."
	}
	_, err = fmt.Fprintf(out, "%s\nСледующая реплика: lawa reply %s --root %q --task-file <файл>\n", runstore.SafeTerminalText(result), snapshot.Meta.RunID, root)
	return err
}
