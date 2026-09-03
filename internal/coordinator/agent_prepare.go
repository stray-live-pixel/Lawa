package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/stray-live-pixel/Lawa/internal/capacity"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

const chooseDecisionToolName = "choose_decision"

var errAgentRunBecameTerminal = errors.New("agent-graph завершился во время подготовки")

// agentTerminalPreparationError отделяет управляющий terminal-сигнал от ошибки
// освобождения capacity. Execution обязан завершить и прервать active turn, а
// затем присоединить Cleanup к итоговой диагностике, не маскируя её sentinel.
type agentTerminalPreparationError struct{ Cleanup error }

func (e *agentTerminalPreparationError) Error() string { return errAgentRunBecameTerminal.Error() }
func (e *agentTerminalPreparationError) Unwrap() error { return errAgentRunBecameTerminal }

// agentWorkKind отделяет создание нового Codex-чата от нового turn уже
// сохранённого чата. Оба вида работы занимают одинаковый slot параллельности и
// адресуются visitId; stepId после появления циклов перестаёт быть уникальным.
type agentWorkKind uint8

const (
	agentWorkLaunch agentWorkKind = iota + 1
	agentWorkContinuation
)

// agentWork — полностью подготовленный сетевой запрос одного посещения. Lease
// передаётся будущему execution-циклу и должен удерживаться до terminal result.
// Для continuation ThreadID содержит уже сохранённый Codex thread; новый launch
// получает его только через OnThread и поэтому возвращается с пустым ThreadID.
type agentWork struct {
	VisitID, StepID, ThreadID string
	Command                   codex.Command
	kind                      agentWorkKind
	lease                     *capacity.Lease
}

// agentPreparation сохраняет единый FIFO и для новых, и для продолженных visits.
// WaitingForCapacity содержит visitId, а не stepId: один и тот же логический
// кубик может иметь несколько независимых посещений в durable-истории.
type agentPreparation struct {
	Work               []agentWork
	WaitingForCapacity []string
}

// prepareAgentVisits строит команды всех доступных Pending/Cancelled visits,
// получает для начала FIFO доступные root-wide slots и только после этого одним
// durable commit переводит выбранные Pending в Starting. До успешного ReserveVisits
// вызывающий код не имеет права передавать новый launch клиенту Codex.
//
// Вызывающий код обязан непосредственно перед подготовкой выполнить
// AdvanceAgentGraph и остановиться на его terminal результате. Prepare намеренно
// не дублирует planner: иначе unapplied finish мог бы соревноваться с запуском
// уже существующего Pending visit, а две реализации переходов начали бы расходиться.
//
// Cancelled выбирается лишь для явного resume и не меняется на диске заранее:
// связь с прежним чатом уже сохранена. Карта continued ограничивает один continue
// на visit за один execution-процесс; функция её не меняет, потому что владение
// начинается только когда вызывающий код зарегистрировал activeExecution.
func prepareAgentVisits(
	run *runstore.LockedRun,
	root string,
	pool *capacity.Pool,
	continueCancelled bool,
	continued map[string]bool,
	configure func(runstore.Snapshot, *codex.Command),
) (agentPreparation, error) {
	if run == nil {
		return agentPreparation{}, errors.New("координатор agent-graph: нужен открытый запуск")
	}
	snapshot, err := run.Load()
	if err != nil {
		return agentPreparation{}, fmt.Errorf("координатор agent-graph: прочитать запуск: %w", err)
	}
	if snapshot.Meta.Version != 4 || snapshot.Workflow.EffectiveVersion() != workflow.VersionAgentGraph {
		return agentPreparation{}, errors.New("координатор agent-graph: подготовка требует meta.json v4 и workflow version=2")
	}
	if snapshot.Meta.RunState != runstore.RunRunning {
		return agentPreparation{}, errors.New("координатор agent-graph: завершённый run не принимает новые turn")
	}
	runDir, err := validateAgentRoot(run, snapshot, root)
	if err != nil {
		return agentPreparation{}, err
	}
	if pool == nil {
		pool = capacity.Unlimited()
	}

	steps := make(map[string]workflow.Step, len(snapshot.Workflow.Steps))
	for _, step := range snapshot.Workflow.Steps {
		steps[step.ID] = step
	}
	// Команды строятся до получения slots и durable reserve. Поэтому конфликт
	// встроенного имени инструмента либо повреждённая причинная ссылка не оставит
	// Starting, хотя ни один сетевой запрос ещё даже не мог начаться.
	candidates := make([]agentWork, 0)
	for _, visit := range snapshot.Meta.Visits {
		kind := agentWorkKind(0)
		switch {
		case visit.State == scheduler.Pending:
			kind = agentWorkLaunch
		case visit.State == scheduler.Cancelled && continueCancelled && !continued[visit.VisitID]:
			kind = agentWorkContinuation
		default:
			continue
		}
		step, exists := steps[visit.StepID]
		if !exists {
			return agentPreparation{}, fmt.Errorf("координатор agent-graph: посещение %q ссылается на неизвестный шаг %q", visit.VisitID, visit.StepID)
		}
		prompt, promptErr := buildAgentPrompt(snapshot, step, visit, runDir)
		if promptErr != nil {
			return agentPreparation{}, promptErr
		}
		ownMemory := filepath.Join(runDir, "memory", visit.VisitID+".md")
		command := codex.Command{
			CWD:         snapshot.Meta.CWD,
			Title:       fmt.Sprintf("Lawa: %s / %s #%d, итерация %d [%s]", snapshot.Workflow.ID, visit.StepID, visit.Visit, visit.Iteration, snapshot.Meta.RunID),
			Text:        prompt,
			Permissions: stepPermissions(runDir, ownMemory, visit.VisitID),
		}
		applyRuntimeSettings(&command, snapshot.Workflow.Model, step)
		if configure != nil {
			// Конфигуратор CLI первым добавляет run_child/run_children. Только после
			// него coordinator может завернуть handler, не потеряв дочерние tools.
			configure(snapshot, &command)
		}
		if err = addChooseDecision(run, step, visit, &command); err != nil {
			return agentPreparation{}, fmt.Errorf("координатор agent-graph: посещение %q: %w", visit.VisitID, err)
		}
		candidates = append(candidates, agentWork{
			VisitID: visit.VisitID, StepID: visit.StepID, ThreadID: visit.CodexThreadID,
			Command: command, kind: kind,
		})
	}

	prepared := agentPreparation{}
	for index := range candidates {
		lease, available, acquireErr := pool.TryAcquire()
		if acquireErr != nil {
			return agentPreparation{}, errors.Join(
				fmt.Errorf("координатор agent-graph: получить слот параллельности: %w", acquireErr),
				releaseAgentWork(prepared.Work),
			)
		}
		if !available {
			for _, waiting := range candidates[index:] {
				prepared.WaitingForCapacity = append(prepared.WaitingForCapacity, waiting.VisitID)
			}
			break
		}
		candidate := candidates[index]
		candidate.lease = lease
		prepared.Work = append(prepared.Work, candidate)
	}

	reserved := make([]string, 0, len(prepared.Work))
	for _, work := range prepared.Work {
		if work.kind == agentWorkLaunch {
			reserved = append(reserved, work.VisitID)
		}
	}
	if err = run.ReserveVisits(reserved); err != nil {
		releaseErr := releaseAgentWork(prepared.Work)
		if errors.Is(err, runstore.ErrAgentDecisionPoisoned) {
			advanced, advanceErr := run.AdvanceAgentGraph()
			if advanceErr != nil {
				return agentPreparation{}, errors.Join(
					fmt.Errorf("координатор agent-graph: завершить конфликтующее решение: %w", advanceErr), releaseErr,
				)
			}
			if advanced.Snapshot.Meta.RunState == runstore.RunRunning {
				return agentPreparation{}, errors.Join(
					errors.New("координатор agent-graph: planner не завершил конфликтующее решение"), releaseErr,
				)
			}
			return agentPreparation{}, &agentTerminalPreparationError{Cleanup: releaseErr}
		}
		latest, loadErr := run.Load()
		if loadErr == nil && latest.Meta.RunState != runstore.RunRunning {
			return agentPreparation{}, &agentTerminalPreparationError{Cleanup: releaseErr}
		}
		return agentPreparation{}, errors.Join(
			fmt.Errorf("координатор agent-graph: зарезервировать посещения: %w", err),
			releaseErr, loadErr,
		)
	}
	return prepared, nil
}

// releaseAgentWork возвращает все уже полученные slots при ошибке подготовки.
// Повторный Release безопасен, но нормальный execution вызывает его лишь после
// получения результата соответствующего turn.
func releaseAgentWork(work []agentWork) error {
	var err error
	for _, item := range work {
		err = errors.Join(err, item.lease.Release())
	}
	return err
}

// validateAgentRoot проверяет именно root, переданный execution-циклом. LockedRun
// читает файлы через собственный os.Root, но prompt и permission profile строятся
// из отдельной Options.Root; без этой сверки опечатка дала бы агенту чужие пути.
func validateAgentRoot(run *runstore.LockedRun, snapshot runstore.Snapshot, root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("координатор agent-graph: нужна папка хранения root")
	}
	runDir, err := run.ResolveDirectory(root)
	if err != nil {
		return "", fmt.Errorf("координатор agent-graph: проверить root: %w", err)
	}
	// Проверка каждого leaf не замечает симлинк в memory: Lstat вернул бы уже
	// целевой обычный файл. Поэтому сначала отдельно фиксируем настоящий каталог,
	// а затем проверяем каждую разрешённую агенту память.
	memoryDir := filepath.Join(runDir, "memory")
	memoryInfo, err := os.Lstat(memoryDir)
	if err != nil || !memoryInfo.IsDir() {
		if err == nil {
			err = errors.New("не является настоящим каталогом без симлинка")
		}
		return "", fmt.Errorf("координатор agent-graph: проверить каталог памяти: %w", err)
	}
	for _, visit := range snapshot.Meta.Visits {
		memory := filepath.Join(runDir, "memory", visit.VisitID+".md")
		info, statErr := os.Lstat(memory)
		if statErr != nil || !info.Mode().IsRegular() {
			if statErr == nil {
				statErr = errors.New("не является обычным файлом")
			}
			return "", fmt.Errorf("координатор agent-graph: проверить память посещения %q: %w", visit.VisitID, statErr)
		}
	}
	return runDir, nil
}

// buildAgentPrompt передаёт агенту только durable-факты одного согласованного
// snapshot. Содержимое memory не копируется в prompt: абсолютные пути позволяют
// агенту прочитать нужные результаты без раздувания каждого следующего turn.
// Причинные источники перечислены отдельно и в сохранённом порядке; остальные
// terminal visits дают доступ к истории параллельных веток и будущих итераций.
func buildAgentPrompt(snapshot runstore.Snapshot, step workflow.Step, visit runstore.Visit, runDir string) (string, error) {
	visits := make(map[string]runstore.Visit, len(snapshot.Meta.Visits))
	for _, saved := range snapshot.Meta.Visits {
		visits[saved.VisitID] = saved
	}
	causal := make(map[string]bool, len(visit.Trigger.SourceVisitIDs))
	var sources strings.Builder
	if len(visit.Trigger.SourceVisitIDs) == 0 {
		sources.WriteString("- нет\n")
	}
	for _, sourceID := range visit.Trigger.SourceVisitIDs {
		source, exists := visits[sourceID]
		if !exists {
			return "", fmt.Errorf("координатор agent-graph: посещение %q: нет причинного источника %q", visit.VisitID, sourceID)
		}
		causal[sourceID] = true
		writeAgentVisitContext(&sources, runDir, source)
	}

	var history strings.Builder
	for _, saved := range snapshot.Meta.Visits {
		if saved.VisitID == visit.VisitID || causal[saved.VisitID] || saved.State != scheduler.Succeeded && saved.State != scheduler.Failed {
			continue
		}
		writeAgentVisitContext(&history, runDir, saved)
	}
	if history.Len() == 0 {
		history.WriteString("- нет\n")
	}

	trigger, err := json.Marshal(struct {
		Kind        runstore.TriggerKind `json:"kind"`
		DecisionKey string               `json:"decisionKey,omitempty"`
	}{Kind: visit.Trigger.Kind, DecisionKey: visit.Trigger.DecisionKey})
	if err != nil {
		return "", fmt.Errorf("координатор agent-graph: закодировать trigger: %w", err)
	}
	sections := []string{
		"Ты выполняешь отдельное посещение кубика workflow Lawa.",
		"ID запуска (runId): " + snapshot.Meta.RunID,
		"ID посещения (visitId): " + visit.VisitID,
		"ID логического кубика (stepId): " + visit.StepID,
		fmt.Sprintf("Номер посещения кубика (visit): %d", visit.Visit),
		fmt.Sprintf("Итерация графа (iteration): %d", visit.Iteration),
		fmt.Sprintf("Номер начинаемого turn этого посещения (attempt): %d", visit.Attempt+1),
		"Сохранённое состояние перед новым turn: " + string(visit.State),
		"Причина активации (trigger): " + string(trigger),
		"",
		"Общий вход запуска:",
		snapshot.Task,
		"Задача этого кубика:",
		step.Prompt,
		"",
		"Собственная память; прочитай её перед работой и обновляй только этот файл:",
		filepath.Join(runDir, "memory", visit.VisitID+".md"),
		"",
		"Причинные источники в порядке trigger; статус и диагностика ниже являются техническими фактами, бизнес-вывод сделай сам:",
		strings.TrimSuffix(sources.String(), "\n"),
		"",
		"Другие завершённые посещения; их память доступна только для чтения:",
		strings.TrimSuffix(history.String(), "\n"),
		"",
		"Не изменяй чужую память, workflow.json, task.md, meta.json и coordinator.lock в папке запуска.",
		"Если задача требует дочерний workflow, используй только доступные встроенные run_child/run_children, а не shell-команду lawa run.",
		"Перед завершением запиши в собственную память итог, пути к результатам и оставшиеся ограничения.",
	}
	if visit.TechnicalError != "" {
		sections = append(sections, "Техническая диагностика предыдущего turn этого посещения: "+visit.TechnicalError)
	}
	if len(step.Decisions) != 0 {
		keys := sortedAgentDecisionKeys(step.Decisions)
		encodedKeys, marshalErr := json.Marshal(keys)
		if marshalErr != nil {
			return "", fmt.Errorf("координатор agent-graph: закодировать решения: %w", marshalErr)
		}
		if visit.Decision == nil {
			sections = append(sections,
				"Этот кубик обязан выбрать ровно одно решение из JSON-массива: "+string(encodedKeys),
				"Перед успешным завершением ровно один раз вызови встроенный choose_decision; обычный текст ответа не выбирает маршрут.",
			)
		} else {
			encodedDecision, marshalErr := json.Marshal(visit.Decision.Key)
			if marshalErr != nil {
				return "", fmt.Errorf("координатор agent-graph: закодировать сохранённое решение: %w", marshalErr)
			}
			sections = append(sections,
				"Решение уже устойчиво сохранено предыдущим turn: "+string(encodedDecision)+".",
				"Не вызывай choose_decision повторно; закончи оставшуюся работу и сохрани память.",
			)
			if visit.Decision.Explanation != "" {
				sections = append(sections, "Сохранённое объяснение решения: "+visit.Decision.Explanation)
			}
		}
	}
	return strings.Join(sections, "\n"), nil
}

// writeAgentVisitContext добавляет одну безопасную строку истории. TechnicalError
// уже ограничена и очищена валидатором meta v4; решение кодируется как JSON,
// чтобы допустимые пробелы в key не меняли структуру служебного текста.
func writeAgentVisitContext(target *strings.Builder, runDir string, visit runstore.Visit) {
	decision := ""
	if visit.Decision != nil {
		encoded, _ := json.Marshal(visit.Decision.Key)
		decision = ", решение=" + string(encoded)
		if visit.Decision.Explanation != "" {
			decision += ", объяснение=" + visit.Decision.Explanation
		}
	}
	diagnostic := ""
	if visit.TechnicalError != "" {
		diagnostic = ", техническая диагностика=" + visit.TechnicalError
	}
	fmt.Fprintf(target, "- step=%s, visit=%d, iteration=%d, visitId=%s, state=%s%s%s, memory=%s\n",
		visit.StepID, visit.Visit, visit.Iteration, visit.VisitID, visit.State, diagnostic, decision,
		filepath.Join(runDir, "memory", visit.VisitID+".md"))
}

// addChooseDecision резервирует имя graph-control tool после внешнего
// ConfigureCommand. Для обычного кубика и для уже сохранённого выбора команда
// остаётся без этого инструмента. Captured handler вызывается для любого другого
// имени ровно один раз, поэтому run_child/run_children не дублируются.
func addChooseDecision(run *runstore.LockedRun, step workflow.Step, visit runstore.Visit, command *codex.Command) error {
	for _, tool := range command.DynamicTools {
		if tool.Name == chooseDecisionToolName {
			return fmt.Errorf("ConfigureCommand занял служебное имя %q", chooseDecisionToolName)
		}
	}
	if len(command.DynamicTools) != 0 && command.CallDynamicTool == nil {
		return errors.New("ConfigureCommand добавил dynamic tools без обработчика")
	}
	if len(step.Decisions) == 0 || visit.Decision != nil {
		return nil
	}
	keys := sortedAgentDecisionKeys(step.Decisions)
	schema, err := json.Marshal(struct {
		Type                 string `json:"type"`
		AdditionalProperties bool   `json:"additionalProperties"`
		Properties           struct {
			Decision struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			} `json:"decision"`
			Explanation struct {
				Type      string `json:"type"`
				MaxLength int    `json:"maxLength"`
			} `json:"explanation"`
		} `json:"properties"`
		Required []string `json:"required"`
	}{
		Type: "object", AdditionalProperties: false, Required: []string{"decision"},
		Properties: struct {
			Decision struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			} `json:"decision"`
			Explanation struct {
				Type      string `json:"type"`
				MaxLength int    `json:"maxLength"`
			} `json:"explanation"`
		}{
			Decision: struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			}{Type: "string", Enum: keys},
			Explanation: struct {
				Type      string `json:"type"`
				MaxLength int    `json:"maxLength"`
			}{Type: "string", MaxLength: 4096},
		},
	})
	if err != nil {
		return fmt.Errorf("построить схему choose_decision: %w", err)
	}
	command.DynamicTools = append(command.DynamicTools, codex.DynamicTool{
		Name:        chooseDecisionToolName,
		Description: "Устойчиво сохранить ровно одно разрешённое решение текущего посещения; маршрут будет применён координатором после успешного завершения turn.",
		InputSchema: schema,
	})
	previous := command.CallDynamicTool
	command.CallDynamicTool = func(ctx context.Context, call codex.DynamicToolCall) (string, error) {
		if call.Tool != chooseDecisionToolName {
			if previous == nil {
				return "", fmt.Errorf("неподдерживаемый dynamic tool %q", call.Tool)
			}
			return previous(ctx, call)
		}
		var input struct {
			Decision    string `json:"decision"`
			Explanation string `json:"explanation,omitempty"`
		}
		if err := json.Unmarshal(call.Arguments, &input, json.RejectUnknownMembers(true)); err != nil {
			return "", fmt.Errorf("прочитать choose_decision: %w", err)
		}
		record, err := run.CommitDecision(
			visit.VisitID, call.ThreadID, call.TurnID,
			input.Decision, input.Explanation, call.CallID,
		)
		if err != nil {
			return "", err
		}
		response, err := json.Marshal(struct {
			Committed bool                      `json:"committed"`
			Decision  string                    `json:"decision"`
			To        []string                  `json:"to,omitempty"`
			Finish    *workflow.TerminalOutcome `json:"finish,omitempty"`
		}{Committed: true, Decision: record.Key, To: slices.Clone(record.To), Finish: record.Finish})
		if err != nil {
			return "", fmt.Errorf("подтвердить choose_decision: %w", err)
		}
		return string(response), nil
	}
	return nil
}

// sortedAgentDecisionKeys исключает зависимость schema и prompt от порядка map.
// Сравнение ключей остаётся точным: пробелы не обрезаются и регистр не меняется.
func sortedAgentDecisionKeys(decisions map[string]workflow.Route) []string {
	keys := make([]string, 0, len(decisions))
	for key := range decisions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
