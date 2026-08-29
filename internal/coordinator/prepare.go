// Package coordinator связывает сохранённый запуск с планировщиком и клиентом
// Codex. Этот файл отвечает только за безопасную подготовку новых задач: сетевые
// запросы и наблюдение за уже созданными чатами добавляются отдельными этапами.
package coordinator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stray-live-pixel/flows-2/internal/codex"
	"github.com/stray-live-pixel/flows-2/internal/runstore"
	"github.com/stray-live-pixel/flows-2/internal/scheduler"
	"github.com/stray-live-pixel/flows-2/internal/workflow"
)

// Launch — один зарезервированный сетевой запрос. StepID остаётся внутренним ID
// графа, а Command содержит уже собранный пользовательский ввод для нового чата.
// Вызывающий код обязан сохранить CodexThreadID через LockedRun.Update в OnThread.
type Launch struct {
	StepID  string
	Command codex.Command
}

// Preparation описывает решение по одному целостному снимку. Waiting показывает
// ещё не запущенные зависимости, Complete — уже завершённый workflow. Launches
// содержит только шаги, которые Prepare успел сохранить как Starting.
type Preparation struct {
	Launches []Launch
	Waiting  []string
	Complete bool
}

// Prepare выбирает готовые Pending-шаги и атомарно сохраняет намерение создать
// всю готовую волну до возврата команд вызывающему коду. Благодаря этому ни
// повторный Prepare, ни перезапуск процесса не создаст второй чат вслепую.
//
// Если запись не удалась, функция не возвращает список для запуска. Атомарный
// meta.json не публикует частичную волну; LockedRun после ошибки Sync всё равно
// запрещает операции, потому что результат публикации мог стать неопределённым.
func Prepare(run *runstore.LockedRun, root string) (Preparation, error) {
	if run == nil {
		return Preparation{}, fmt.Errorf("координатор: нужен открытый запуск")
	}
	if root == "" {
		return Preparation{}, fmt.Errorf("координатор: нужна папка хранения root")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Preparation{}, fmt.Errorf("координатор: определить root: %w", err)
	}
	snapshot, err := run.Load()
	if err != nil {
		return Preparation{}, fmt.Errorf("координатор: прочитать запуск: %w", err)
	}
	states := make(map[string]scheduler.State, len(snapshot.Meta.Steps))
	steps := make(map[string]workflow.Step, len(snapshot.Workflow.Steps))
	for _, step := range snapshot.Meta.Steps {
		states[step.ID] = step.State
	}
	for _, step := range snapshot.Workflow.Steps {
		steps[step.ID] = step
	}
	// Root приходит от CLI отдельно от LockedRun. До резервирования проверяем,
	// что он действительно указывает на этот run: иначе агент получил бы пути
	// несуществующей памяти, а состояние уже нельзя было бы вернуть в Pending.
	for _, step := range snapshot.Meta.Steps {
		memory := filepath.Join(root, snapshot.Meta.RunID, "memory", step.ThreadID+".md")
		info, statErr := os.Stat(memory)
		if statErr != nil || !info.Mode().IsRegular() {
			if statErr == nil {
				statErr = fmt.Errorf("не является обычным файлом")
			}
			return Preparation{}, fmt.Errorf("координатор: проверить память шага %q: %w", step.ID, statErr)
		}
	}
	plan, err := scheduler.Evaluate(snapshot.Workflow, states)
	if err != nil {
		return Preparation{}, fmt.Errorf("координатор: %w", err)
	}
	prepared := Preparation{Waiting: plan.Waiting, Complete: plan.Complete}
	if err := run.Reserve(plan.Ready); err != nil {
		return Preparation{}, fmt.Errorf("координатор: зарезервировать готовые шаги: %w", err)
	}
	for _, stepID := range plan.Ready {
		prepared.Launches = append(prepared.Launches, Launch{
			StepID: stepID,
			Command: codex.Command{
				CWD:   snapshot.Meta.CWD,
				Title: "Lawa: " + snapshot.Workflow.ID + " / " + stepID,
				Text:  buildPrompt(snapshot, steps[stepID], root),
			},
		})
	}
	return prepared, nil
}

// buildPrompt разделяет неизменяемую постановку, локальную задачу кубика и
// служебный контракт. Пути абсолютны: чат может пережить процесс Lawa и не должен
// зависеть от его текущей директории. Чужая память перечислена только для чтения;
// единственный разрешённый файл записи назван отдельно и недвусмысленно.
func buildPrompt(snapshot runstore.Snapshot, step workflow.Step, root string) string {
	runDir := filepath.Join(root, snapshot.Meta.RunID)
	memories := make([]string, 0, len(snapshot.Meta.Steps))
	var ownMemory string
	for _, saved := range snapshot.Meta.Steps {
		path := filepath.Join(runDir, "memory", saved.ThreadID+".md")
		memories = append(memories, fmt.Sprintf("- %s: %s", saved.ID, path))
		if saved.ID == step.ID {
			ownMemory = path
		}
	}
	return strings.Join([]string{
		"Ты выполняешь кубик workflow Lawa.",
		"",
		"Общий вход запуска:",
		snapshot.Task,
		"Задача этого кубика:",
		step.Prompt,
		"",
		"Память кубиков:",
		strings.Join(memories, "\n"),
		"",
		"Перед началом прочитай свою память: " + ownMemory,
		"По ходу работы обновляй только этот файл. Чужую память можно читать, но нельзя изменять.",
		"Не изменяй workflow.json, task.md, meta.json и coordinator.lock в папке запуска.",
		"Перед завершением запиши в свою память итог, пути к результатам и оставшиеся ограничения.",
	}, "\n")
}
