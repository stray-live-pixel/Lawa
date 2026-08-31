// Package appdriver хранит детерминированную часть app-native исполнения.
// Создание задач, чтение живых событий и отправка сообщений остаются у Codex App:
// пакет только выдаёт следующий кубик и атомарно связывает результат app-инструмента
// с run. Поэтому Lawa не поднимает второй app-server и не дублирует интерфейс чата.
package appdriver

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/coordinator"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// Action — один самодостаточный ответ команды app-next. Kind принимает:
// launch — найти либо создать и привязать ровно одну задачу; observe — наблюдать
// уже известные задачи; complete — все зависимости успешны; blocked — новых
// действий нет, но workflow не завершён. Waiting содержит только Pending-кубики,
// ожидающие успешных зависимостей.
type Action struct {
	Kind    string   `json:"kind"`
	RunID   string   `json:"runId"`
	Launch  *Launch  `json:"launch,omitempty"`
	Tasks   []Task   `json:"tasks,omitempty"`
	Waiting []string `json:"waiting,omitempty"`
}

// Launch описывает задачу Codex App без transport-specific параметров stdio.
// Пустой CodexThreadID означает, что task ещё не привязан. Непустой ID делает
// повторный app-next восстановлением той же задачи, а не разрешением создать новую.
type Launch struct {
	StepID        string `json:"stepId"`
	Title         string `json:"title"`
	Prompt        string `json:"prompt"`
	CodexThreadID string `json:"codexThreadId,omitempty"`
	Model         string `json:"model,omitempty"`
	Effort        string `json:"effort,omitempty"`
	Revision      uint64 `json:"revision"`
}

// Task — сохранённая identity задачи, которую управляющий чат передаёт
// read_thread/wait_threads. State — состояние Lawa, не статус UI Codex App.
// Revision надо без изменений вернуть в app-update: только так хранилище может
// отличить наблюдение этого снимка от запоздавшего результата другого наблюдателя.
type Task struct {
	StepID        string          `json:"stepId"`
	CodexThreadID string          `json:"codexThreadId"`
	State         scheduler.State `json:"state"`
	Revision      uint64          `json:"revision"`
}

// Next удерживает lock только на время локального решения. Сначала возвращается
// незавершённая привязка Starting: task нужно найти по точному title либо создать
// один раз и сохранить ID. Только после её разрешения резервируется новый кубик.
func Next(root, runID string) (action Action, err error) {
	run, err := runstore.OpenLocked(root, runID)
	if err != nil {
		return Action{}, fmt.Errorf("app driver: открыть запуск: %w", err)
	}
	defer func() { err = errors.Join(err, run.Close()) }()
	snapshot, err := run.Load()
	if err != nil {
		return Action{}, fmt.Errorf("app driver: прочитать запуск: %w", err)
	}
	for _, step := range snapshot.Meta.Steps {
		if step.State != scheduler.Starting {
			continue
		}
		launch, buildErr := coordinator.AppLaunch(snapshot, root, step.ID)
		if buildErr != nil {
			return Action{}, buildErr
		}
		return Action{Kind: "launch", RunID: runID, Launch: actionLaunch(launch, step.CodexThreadID, step.Revision)}, nil
	}
	prepared, err := coordinator.PrepareNextForApp(run, root)
	if err != nil {
		return Action{}, err
	}
	if len(prepared.Launches) == 1 {
		return Action{Kind: "launch", RunID: runID, Launch: actionLaunch(prepared.Launches[0], "", 0)}, nil
	}
	if prepared.Complete {
		return Action{Kind: "complete", RunID: runID}, nil
	}
	for _, step := range snapshot.Meta.Steps {
		if step.CodexThreadID == "" || step.State == scheduler.Pending || step.State == scheduler.Succeeded {
			continue
		}
		action.Tasks = append(action.Tasks, Task{StepID: step.ID, CodexThreadID: step.CodexThreadID, State: step.State, Revision: step.Revision})
	}
	action.RunID, action.Waiting = runID, prepared.Waiting
	if len(action.Tasks) != 0 {
		action.Kind = "observe"
	} else {
		action.Kind = "blocked"
	}
	return action, nil
}

func actionLaunch(launch coordinator.Launch, threadID string, revision uint64) *Launch {
	return &Launch{
		StepID: launch.StepID, Title: launch.Command.Title, Prompt: launch.Command.Text,
		CodexThreadID: threadID, Model: launch.Command.Model, Effort: launch.Command.Effort, Revision: revision,
	}
}

// Claim выдаёт одному управляющему чату устойчивое право вызвать create_thread.
// False означает, что попытка уже могла уйти в Codex App и разрешён только поиск
// задачи по детерминированному title — даже если list_threads пока ничего не вернул.
func Claim(root, runID, stepID string) (claimed bool, err error) {
	run, err := runstore.OpenLocked(root, runID)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, run.Close()) }()
	return run.ClaimAppCreation(stepID)
}

// ResetClaim выполняет только подтверждённый пользователем аварийный сброс.
// Обычное восстановление всегда ищет прежнюю задачу по title и не вызывает этот
// метод: отсутствие задачи в одном снимке list_threads не доказывает, что внешний
// create_thread не был принят Codex App.
func ResetClaim(root, runID, stepID string) (err error) {
	run, err := runstore.OpenLocked(root, runID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, run.Close()) }()
	return run.ResetAppCreationClaim(stepID)
}

// Bind сохраняет identity сразу после атомарного создания app-задачи с уникальными
// title и первым prompt. Если ответ create потерян, управляющий чат сначала находит
// задачу по title и привязывает её вместо повторного создания. Повтор с тем же ID
// идемпотентен; другой ID запрещён: execution не может незаметно получить дубль.
func Bind(root, runID, stepID, threadID string) (err error) {
	if !validText(threadID) {
		return errors.New("app driver: нужен непустой thread-id UTF-8")
	}
	run, err := runstore.OpenLocked(root, runID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, run.Close()) }()
	snapshot, err := run.Load()
	if err != nil {
		return err
	}
	for _, step := range snapshot.Meta.Steps {
		if step.ID != stepID {
			continue
		}
		if step.CodexThreadID == threadID {
			return nil
		}
		if step.State != scheduler.Starting || step.CodexThreadID != "" {
			return fmt.Errorf("app driver: шаг %q не ожидает привязку задачи", stepID)
		}
		return run.Update(stepID, scheduler.Starting, threadID)
	}
	return fmt.Errorf("app driver: нет шага %q", stepID)
}

// Update сохраняет наблюдаемый Codex App статус той же задачи. Для succeeded
// финальный ответ обязателен и сначала атомарно становится памятью кубика; лишь
// затем состояние разрешает запуск зависимостей. Это сохраняет порядок данных
// после сбоя между двумя записями: лишняя память безопаснее преждевременного успеха.
func Update(root, runID, stepID, state string, revision uint64, result []byte) (err error) {
	next, err := parseState(state)
	if err != nil {
		return err
	}
	if next == scheduler.Succeeded && (!utf8.Valid(result) || strings.TrimSpace(string(result)) == "") {
		return errors.New("app driver: succeeded требует непустой финальный ответ UTF-8")
	}
	if len(result) != 0 && !utf8.Valid(result) {
		return errors.New("app driver: результат должен быть UTF-8")
	}
	run, err := runstore.OpenLocked(root, runID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, run.Close()) }()
	snapshot, err := run.Load()
	if err != nil {
		return err
	}
	threadID, savedState, savedRevision := "", scheduler.State(""), uint64(0)
	for _, step := range snapshot.Meta.Steps {
		if step.ID == stepID {
			threadID, savedState, savedRevision = step.CodexThreadID, step.State, step.Revision
			break
		}
	}
	if threadID == "" {
		return fmt.Errorf("app driver: шаг %q ещё не привязан к задаче", stepID)
	}
	if savedRevision != revision {
		return fmt.Errorf("app driver: шаг %q: %w: ожидалась %d, сохранена %d", stepID, runstore.ErrRevisionConflict, revision, savedRevision)
	}
	if savedState == scheduler.Succeeded {
		return fmt.Errorf("app driver: шаг %q уже финализирован", stepID)
	}
	if len(result) != 0 {
		if err = run.WriteMemory(stepID, result); err != nil {
			return err
		}
	}
	return run.UpdateIfRevision(stepID, next, threadID, revision)
}

func parseState(value string) (scheduler.State, error) {
	switch scheduler.State(value) {
	case scheduler.Unknown, scheduler.Running, scheduler.WaitingForApproval,
		scheduler.Failed, scheduler.Cancelled, scheduler.Succeeded:
		return scheduler.State(value), nil
	default:
		return "", fmt.Errorf("app driver: неизвестное состояние %q", value)
	}
}

func validText(value string) bool { return utf8.ValidString(value) && strings.TrimSpace(value) != "" }
