// Package coordinator связывает сохранённый запуск с планировщиком и клиентом
// Codex. Prepare безопасно резервирует новые задачи, а Execute запускает волны,
// наблюдает сохранённые чаты и продолжает workflow после ручной работы.
package coordinator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
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

// Continuation описывает один новый turn в уже существующем чате. Он создаётся
// только для Cancelled при явном resume и никогда не меняет CodexThreadID.
type Continuation struct {
	StepID, ThreadID string
	Command          codex.Command
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
	savedSteps := make(map[string]runstore.Step, len(snapshot.Meta.Steps))
	for _, step := range snapshot.Meta.Steps {
		states[step.ID] = step.State
		savedSteps[step.ID] = step
	}
	for _, step := range snapshot.Workflow.Steps {
		steps[step.ID] = step
	}
	// Root приходит от CLI отдельно от LockedRun. До резервирования проверяем,
	// что он действительно указывает на этот run: иначе агент получил бы пути
	// несуществующей памяти. Все детерминированные локальные ошибки должны быть
	// найдены до публикации Starting, не заставляя механизм сетевого восстановления
	// снимать резервирование, которое вообще не требовалось.
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
		saved := savedSteps[stepID]
		workflowStep := steps[stepID]
		runDir := filepath.Join(root, snapshot.Meta.RunID)
		ownMemory := filepath.Join(runDir, "memory", saved.ThreadID+".md")
		command := codex.Command{
			CWD:         snapshot.Meta.CWD,
			Title:       fmt.Sprintf("Lawa: %s / %s [%s]", snapshot.Workflow.ID, stepID, snapshot.Meta.RunID),
			Text:        buildPrompt(snapshot, workflowStep, saved, root),
			Permissions: stepPermissions(runDir, ownMemory, saved.ThreadID),
		}
		applyRuntimeSettings(&command, snapshot.Workflow.Model, workflowStep)
		prepared.Launches = append(prepared.Launches, Launch{
			StepID:  stepID,
			Command: command,
		})
	}
	return prepared, nil
}

// prepareContinuations выбирает только interrupted/Cancelled-чаты. Карта already
// ограничивает один автоматический continue на один вызов Execute: повторный
// interrupted не превращается в бесконечный цикл, но следующий явный resume снова
// получает право продолжить незавершённую работу.
func prepareContinuations(snapshot runstore.Snapshot, root string, enabled bool, already map[string]bool) ([]Continuation, error) {
	if !enabled {
		return nil, nil
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("координатор: определить root для продолжения: %w", err)
	}
	runDir := filepath.Join(root, snapshot.Meta.RunID)
	workflowSteps := make(map[string]workflow.Step, len(snapshot.Workflow.Steps))
	for _, step := range snapshot.Workflow.Steps {
		workflowSteps[step.ID] = step
	}
	var continuations []Continuation
	for _, step := range snapshot.Meta.Steps {
		if step.State != scheduler.Cancelled || already[step.ID] {
			continue
		}
		memory := filepath.Join(runDir, "memory", step.ThreadID+".md")
		info, statErr := os.Stat(memory)
		if statErr != nil || !info.Mode().IsRegular() {
			if statErr == nil {
				statErr = fmt.Errorf("не является обычным файлом")
			}
			return nil, fmt.Errorf("координатор: проверить память шага %q перед продолжением: %w", step.ID, statErr)
		}
		command := codex.Command{
			CWD: snapshot.Meta.CWD, Text: "continue",
			Permissions: stepPermissions(runDir, memory, step.ThreadID),
		}
		applyRuntimeSettings(&command, snapshot.Workflow.Model, workflowSteps[step.ID])
		continuations = append(continuations, Continuation{
			StepID: step.ID, ThreadID: step.CodexThreadID,
			Command: command,
		})
	}
	return continuations, nil
}

// applyRuntimeSettings переводит устойчивые пользовательские значения workflow
// в текущие имена протокола Codex. Корневой model служит значением по умолчанию,
// а model кубика имеет больший приоритет. Если оба отсутствуют, пустое поле Command
// сохраняет модель из конфигурации Codex. Явный normal обязан отключить унаследованный
// Fast mode, поэтому он передаётся как default; отсутствие speed не добавляет
// serviceTier и сохраняет конфигурацию окружения пользователя.
func applyRuntimeSettings(command *codex.Command, workflowModel *string, step workflow.Step) {
	if workflowModel != nil {
		command.Model = *workflowModel
	}
	if step.Model != nil {
		command.Model = *step.Model
	}
	if step.Effort != nil {
		command.Effort = *step.Effort
	}
	if step.Speed != nil {
		switch *step.Speed {
		case workflow.SpeedNormal:
			command.ServiceTier = "default"
		case workflow.SpeedFast:
			command.ServiceTier = "fast"
		}
	}
}

func stepPermissions(runDir, ownMemory, threadID string) *codex.PermissionProfile {
	return &codex.PermissionProfile{
		Name: "lawa-" + threadID, ReadPaths: []string{runDir}, WritePaths: []string{ownMemory},
	}
}

// buildPrompt разделяет неизменяемую постановку, локальную задачу кубика и
// служебный контракт. Пути абсолютны: чат может пережить процесс Lawa и не должен
// зависеть от его текущей директории. Чужая память перечислена только для чтения;
// единственный разрешённый файл записи назван отдельно и недвусмысленно.
func buildPrompt(snapshot runstore.Snapshot, step workflow.Step, savedStep runstore.Step, root string) string {
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
		"ID запуска (runId): " + snapshot.Meta.RunID,
		"ID этого кубика в run (threadId Lawa): " + savedStep.ThreadID,
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
