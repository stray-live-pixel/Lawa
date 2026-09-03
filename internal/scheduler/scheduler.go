// Package scheduler выбирает готовые шаги по полному снимку запуска, не создавая
// чатов и не меняя состояние. Это независимое от Codex ядро координатора.
package scheduler

import (
	"fmt"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// State объединяет фазу создания чата и последний известный статус его работы.
// Это внутренние состояния Lawa, а не значения протокола Codex. Будущий адаптер
// обязан учитывать актуальное продолжение чата: прежний успех не заменяет статус
// новой работы. Нулевое значение недопустимо, чтобы пропуск не означал новый запуск.
type State string

const (
	// Pending означает, что намерение создать чат ещё не фиксировалось.
	Pending State = "pending"
	// Starting означает сохранённое намерение создать чат. Даже без полученного
	// ID повторный запрос запрещён: сначала координатор должен восстановить связь.
	Starting State = "starting"
	// Unknown означает уже созданный чат без подтверждённого текущего статуса,
	// например после потери связи. Он не разрешает ни повтор, ни запуск зависимостей.
	Unknown State = "unknown"
	// Running означает выполняющуюся работу в уже созданном чате.
	Running State = "running"
	// WaitingForApproval означает ожидание пользователя, а не успешное завершение.
	WaitingForApproval State = "waiting_for_approval"
	// Failed оставляет legacy dependsOn в ожидании. Для v2 это терминальный visit,
	// который может удовлетворить технический after, но сам не создаёт route.
	Failed State = "failed"
	// Cancelled не удовлетворяет зависимость и не запускает автоматический повтор.
	// Явный resume может продолжить тот же чат этого legacy-шага или v2 visit.
	Cancelled State = "cancelled"
	// Skipped — терминальное состояние только посещения workflow v2. Оно означает,
	// что агент не запускался: до этого кубика не дошёл выбранный маршрут либо все
	// его after-источники тоже были пропущены. Legacy dependsOn это состояние не
	// принимает, чтобы изменение v2 не меняло контракт сохранённых DAG.
	Skipped State = "skipped"
	// Succeeded — единственное состояние, удовлетворяющее legacy dependsOn. В v2
	// технический after принимает Succeeded, Failed и Skipped.
	Succeeded State = "succeeded"
)

// Plan описывает решение только для переданного снимка. Ready и Waiting содержат
// ID ещё не начатых шагов в порядке Steps: первые готовы, вторые ждут зависимостей.
// Порядок нужен для воспроизводимости, но не задаёт последовательное исполнение:
// все Ready можно передать на запуск сразу. Уже начатых шагов в обоих списках нет.
type Plan struct {
	Ready    []string
	Waiting  []string
	Complete bool // Все шаги успешны именно в текущем снимке, а не когда-либо в прошлом.
}

// Evaluate проверяет граф и снимок целиком, затем выбирает Pending-шаги, у которых
// все непосредственные зависимости успешны. Ошибка возвращает пустой Plan, даже
// если часть графа могла бы выполняться. Снимок обязан явно содержать состояние
// каждого шага и только его: отсутствующий ключ не считается Pending. Для нового
// запуска вызывающий код должен сам заполнить все состояния значением Pending.
//
// Функция не резервирует Ready. Перед запросом создания чата координатор обязан
// сохранить Starting и передать обновлённый снимок следующему вызову; сбой записи
// запрещает запрос. Повтор с прежним снимком даст тот же план, а не гарантию
// однократного создания чата. Блокировка run и восстановление связи — вне пакета.
//
// Успех не запоминается навсегда: Running после Succeeded снова блокирует ещё
// не начатые зависимые шаги. Уже начатые шаги не отменяются и не пересчитываются.
// Время — O(V + E), включая общую валидацию. Входы не изменяются и не сохраняются;
// вызывающий код не должен менять их параллельно с Evaluate.
func Evaluate(w workflow.Workflow, states map[string]State) (Plan, error) {
	if err := w.Validate(); err != nil {
		return Plan{}, fmt.Errorf("планировщик: %w", err)
	}
	// Evaluate обслуживает только legacy DAG. Workflow v2 планирует отдельный
	// PlanAgentGraph поверх visits; явный отказ не позволяет случайно принять
	// after/decisions как пустой DAG и запустить все шаги одновременно.
	if w.EffectiveVersion() == workflow.VersionAgentGraph {
		return Plan{}, fmt.Errorf("legacy-планировщик не принимает workflow version=2")
	}
	if len(states) != len(w.Steps) {
		return Plan{}, fmt.Errorf("планировщик: нужен снимок ровно для всех %d шагов; состояний: %d", len(w.Steps), len(states))
	}
	// Сначала проверяем весь снимок. Равенства размеров недостаточно: неизвестный
	// ключ может подменить один из настоящих шагов, сохранив прежнее число записей.
	for _, step := range w.Steps {
		state, exists := states[step.ID]
		if !exists {
			return Plan{}, fmt.Errorf("планировщик: нет состояния шага %q", step.ID)
		}
		switch state {
		case Pending, Starting, Unknown, Running, WaitingForApproval, Failed, Cancelled, Succeeded:
		default:
			return Plan{}, fmt.Errorf("планировщик: шаг %q: неизвестное состояние %q", step.ID, state)
		}
	}
	plan := Plan{Complete: true}
	for _, step := range w.Steps {
		state := states[step.ID]
		if state != Succeeded {
			plan.Complete = false
		}
		if state != Pending {
			continue
		}
		ready := true
		for _, dependency := range step.DependsOn {
			if states[dependency] != Succeeded {
				ready = false
				break
			}
		}
		if ready {
			plan.Ready = append(plan.Ready, step.ID)
		} else {
			plan.Waiting = append(plan.Waiting, step.ID)
		}
	}
	return plan, nil
}
