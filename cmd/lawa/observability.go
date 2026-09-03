package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// statusCommand строит read-only снимок без coordinator.lock. Поэтому команда
// работает одновременно с `run`, не влияет на планирование и показывает только
// уже синхронизированные meta.json/events.jsonl.
func statusCommand(args []string, out io.Writer, deps dependencies) error {
	runID, root, err := parseReadRunArguments("status", args, deps)
	if err != nil {
		return err
	}
	snapshot, err := runstore.LoadForDashboard(root, runID)
	if err != nil {
		return fmt.Errorf("прочитать run %q: %w", runID, err)
	}
	events, err := runstore.ReadEvents(root, runID)
	if err != nil {
		return fmt.Errorf("прочитать события run %q: %w", runID, err)
	}
	summaries := runstore.SummarizeEvents(events)
	runtime := "app-server"
	if snapshot.HistoricalAppNative {
		runtime = "app-native (исторический, только чтение)"
	}
	if _, err = fmt.Fprintf(out, "runId: %s\nworkflow: %s\nruntime: %s\ncwd: %s\n",
		runstore.SafeTerminalText(runID), runstore.SafeTerminalText(snapshot.Workflow.ID),
		runstore.SafeTerminalText(runtime), runstore.SafeTerminalText(snapshot.Meta.CWD)); err != nil {
		return err
	}
	if snapshot.Meta.Version == 4 {
		return writeAgentStatus(out, snapshot, summaries)
	}
	for _, step := range snapshot.Meta.Steps {
		summary := summaries[step.ID]
		if err = writeStatusEntry(out, step.ID, step.State, step.CodexThreadID, step.TurnID, summary); err != nil {
			return err
		}
	}
	if snapshot.HistoricalAppNative {
		_, err = fmt.Fprintln(out, "\nЭтот run не возобновляется: его задачи могли быть созданы через Codex Desktop.")
	}
	return err
}

// writeAgentStatus показывает append-only историю v4, не сворачивая повторные
// посещения одного StepID. Статический workflow нужен только для разрешённых
// routes и лимитов; фактический выбор и skipped берутся из durable metadata.
func writeAgentStatus(out io.Writer, snapshot runstore.Snapshot, summaries map[string]runstore.EventSummary) error {
	if _, err := fmt.Fprintf(out, "runState: %s\n", runstore.SafeTerminalText(string(snapshot.Meta.RunState))); err != nil {
		return err
	}
	if snapshot.Meta.StopVisitID != "" {
		if _, err := fmt.Fprintf(out, "stopVisit: %s\n", runstore.SafeTerminalText(snapshot.Meta.StopVisitID)); err != nil {
			return err
		}
	}
	if snapshot.Meta.StopLimitStepID != "" {
		trigger := ""
		if snapshot.Meta.StopLimitTrigger != nil {
			trigger = statusTriggerText(*snapshot.Meta.StopLimitTrigger)
		}
		if _, err := fmt.Fprintf(out, "limit: step=%s, iteration=%d, trigger=%s\n",
			runstore.SafeTerminalText(snapshot.Meta.StopLimitStepID), snapshot.Meta.StopLimitIteration,
			runstore.SafeTerminalText(trigger)); err != nil {
			return err
		}
	}
	if snapshot.Meta.StopReason != "" {
		if _, err := fmt.Fprintf(out, "причина: %s\n", runstore.SafeTerminalText(snapshot.Meta.StopReason)); err != nil {
			return err
		}
	}
	steps := make(map[string]workflow.Step, len(snapshot.Workflow.Steps))
	for _, step := range snapshot.Workflow.Steps {
		steps[step.ID] = step
	}
	for _, visit := range snapshot.Meta.Visits {
		label := fmt.Sprintf("%s#%d [%s]", visit.StepID, visit.Visit, visit.VisitID)
		if err := writeStatusEntry(out, label, visit.State, visit.CodexThreadID, visit.TurnID, summaries[visit.VisitID]); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "  iteration: %d\n  attempt: %d\n  trigger: %s\n",
			visit.Iteration, visit.Attempt, runstore.SafeTerminalText(statusTriggerText(visit.Trigger))); err != nil {
			return err
		}
		step := steps[visit.StepID]
		if step.MaxVisits != nil {
			limit := fmt.Sprintf("maxVisits=%d, onLimit=failed (по умолчанию)", *step.MaxVisits)
			if step.OnLimit != nil {
				limit = fmt.Sprintf("maxVisits=%d, onLimit=%s", *step.MaxVisits, *step.OnLimit)
			}
			if _, err := fmt.Fprintf(out, "  limit: %s\n", runstore.SafeTerminalText(limit)); err != nil {
				return err
			}
		}
		if routes := statusRoutesText(step); routes != "" {
			if _, err := fmt.Fprintf(out, "  routes: %s\n", runstore.SafeTerminalText(routes)); err != nil {
				return err
			}
		}
		if visit.TechnicalError != "" {
			if _, err := fmt.Fprintf(out, "  техническая ошибка: %s\n", runstore.SafeTerminalText(visit.TechnicalError)); err != nil {
				return err
			}
		}
		if visit.Decision != nil {
			decision := visit.Decision
			if _, err := fmt.Fprintf(out, "  решение: %s, applied=%t\n",
				runstore.SafeTerminalText(decision.Key), decision.Applied); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "  переход: %s\n", runstore.SafeTerminalText(statusDecisionDestination(*decision))); err != nil {
				return err
			}
			if decision.Explanation != "" {
				if _, err := fmt.Fprintf(out, "  объяснение: %s\n", runstore.SafeTerminalText(decision.Explanation)); err != nil {
					return err
				}
			}
			if len(decision.Skipped) != 0 {
				if _, err := fmt.Fprintf(out, "  skipped: %s\n", runstore.SafeTerminalText(strings.Join(decision.Skipped, ", "))); err != nil {
					return err
				}
			}
			if decision.Error != "" {
				if _, err := fmt.Fprintf(out, "  ошибка решения: %s\n", runstore.SafeTerminalText(decision.Error)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// writeStatusEntry сохраняет прежний блок runtime-диагностики legacy и даёт v4
// ту же сводку, ключеванную VisitID. Пустые внешние ID показаны явно через "-".
func writeStatusEntry(out io.Writer, label string, state scheduler.State, threadID, turnID string, summary runstore.EventSummary) error {
	if turnID == "" {
		turnID = summary.TurnID
	}
	if threadID == "" {
		threadID = "-"
	}
	if turnID == "" {
		turnID = "-"
	}
	activity, process := "-", "-"
	if !summary.LastActivity.IsZero() {
		activity = summary.LastActivity.Local().Format(time.RFC3339)
	}
	if summary.PID != 0 {
		process = fmt.Sprintf("pid %d", summary.PID)
	} else if summary.ExitCode != nil {
		process = fmt.Sprintf("завершён, exit %d", *summary.ExitCode)
	} else if summary.Signal != "" {
		process = "завершён, signal " + summary.Signal
	}
	if _, err := fmt.Fprintf(out, "\n%s: %s\n  thread: %s\n  turn: %s\n  процесс: %s\n  активность: %s\n",
		runstore.SafeTerminalText(label), runstore.SafeTerminalText(string(state)),
		runstore.SafeTerminalText(threadID), runstore.SafeTerminalText(turnID),
		runstore.SafeTerminalText(process), runstore.SafeTerminalText(activity)); err != nil {
		return err
	}
	if summary.Message != "" {
		if _, err := fmt.Fprintf(out, "  сообщение: %s\n", runstore.SafeTerminalText(summary.Message)); err != nil {
			return err
		}
	}
	if len(summary.ActiveItemTypes) != 0 {
		if _, err := fmt.Fprintf(out, "  действие: %s\n", runstore.SafeTerminalText(strings.Join(summary.ActiveItemTypes, ", "))); err != nil {
			return err
		}
	}
	return nil
}

// statusTriggerText сохраняет порядок causal sourceVisitIds и выбранный ключ.
func statusTriggerText(trigger runstore.VisitTrigger) string {
	result := string(trigger.Kind)
	if trigger.DecisionKey != "" {
		result += ":" + trigger.DecisionKey
	}
	if len(trigger.SourceVisitIDs) != 0 {
		result += " <- " + strings.Join(trigger.SourceVisitIDs, ", ")
	}
	return result
}

// statusRoutesText сортирует decision keys, но не меняет пользовательский
// порядок route.to. Невыбранные routes остаются описанием, а не состояниями.
func statusRoutesText(step workflow.Step) string {
	keys := make([]string, 0, len(step.Decisions))
	for key := range step.Decisions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	routes := make([]string, 0, len(keys))
	for _, key := range keys {
		route := step.Decisions[key]
		destination := strings.Join(route.To, ", ")
		if route.Finish != nil {
			destination = "finish:" + string(*route.Finish)
		}
		routes = append(routes, key+" → "+destination)
	}
	return strings.Join(routes, "; ")
}

// statusDecisionDestination выводит именно сохранённый выбранный переход. Это
// отличает результат visit от полного статического перечня разрешённых routes.
func statusDecisionDestination(decision runstore.DecisionRecord) string {
	if decision.Finish != nil {
		return "finish:" + string(*decision.Finish)
	}
	return strings.Join(decision.To, ", ")
}

// logsCommand печатает только нормализованные события Lawa. `--follow` читает
// новые полные строки с сохранённой byte-позиции и не открывает App Server.
// Терминальный meta.json сам по себе недостаточен для выхода: coordinator пишет
// его раньше финального step_state/visit_state, который иначе мог бы потеряться
// для оператора.
func logsCommand(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	parsed, err := parseLogsArguments(args, deps)
	if err != nil {
		return err
	}
	snapshot, err := runstore.LoadForDashboard(parsed.root, parsed.runID)
	if err != nil {
		return fmt.Errorf("прочитать run %q: %w", parsed.runID, err)
	}
	if err = validateLogsFilter(snapshot, parsed); err != nil {
		return fmt.Errorf("run %q: %w", parsed.runID, err)
	}
	offset := int64(0)
	finalStates := make(map[string]eventState)
	pollInterval := deps.logsPollInterval
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	for {
		var events []runstore.RuntimeEvent
		var readErr error
		if parsed.follow {
			events, offset, readErr = runstore.ReadEventsAfter(parsed.root, parsed.runID, offset)
		} else {
			events, readErr = runstore.ReadEvents(parsed.root, parsed.runID)
		}
		if readErr != nil {
			return fmt.Errorf("прочитать события run %q: %w", parsed.runID, readErr)
		}
		for _, event := range events {
			if stateKey := terminalEventKey(snapshot, event); stateKey != "" {
				finalStates[stateKey] = eventState{State: event.State, TurnID: event.TurnID}
			}
			if parsed.stepID != "" && event.StepID != parsed.stepID ||
				parsed.visitID != "" && event.VisitID != parsed.visitID {
				continue
			}
			if _, err = fmt.Fprintln(out, runstore.FormatEvent(event)); err != nil {
				return err
			}
		}
		if !parsed.follow {
			return nil
		}
		snapshot, loadErr := runstore.LoadForDashboard(parsed.root, parsed.runID)
		if loadErr != nil {
			return fmt.Errorf("прочитать run %q: %w", parsed.runID, loadErr)
		}
		if snapshot.HistoricalAppNative || runTerminal(snapshot) && terminalEventsRecorded(snapshot, finalStates) {
			return nil
		}
		// Пока накопившийся batch не исчерпан, продолжаем сразу. Polling нужен
		// только на настоящем EOF или неполной последней JSONL-строке.
		if len(events) != 0 {
			continue
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type eventState struct{ State, TurnID string }

// terminalEventKey отделяет адреса двух форматов. У v4 stepId намеренно не
// подходит: два прохода цикла имеют один логический шаг, но разные финалы.
func terminalEventKey(snapshot runstore.Snapshot, event runstore.RuntimeEvent) string {
	if snapshot.Meta.Version == 4 {
		if (event.Kind == "visit_state" || event.Kind == "thread_reconciled") && event.VisitID != "" {
			return event.VisitID
		}
		return ""
	}
	if event.Kind == "step_state" || event.Kind == "thread_reconciled" {
		return event.StepID
	}
	return ""
}

// terminalEventsRecorded подтверждает, что журнал дошёл до состояния и turn из
// meta.json каждого кубика. Сравнение turn не даёт старому succeeded-событию
// преждевременно завершить follow после ручного продолжения того же thread.
func terminalEventsRecorded(snapshot runstore.Snapshot, states map[string]eventState) bool {
	// В старых форматах обязательного events.jsonl ещё не было. Их сохранённый
	// терминал остаётся читаемым и не превращает `--follow` в вечное ожидание.
	if snapshot.Meta.Version < 3 {
		return true
	}
	if snapshot.Meta.Version == 4 {
		for _, visit := range snapshot.Meta.Visits {
			// Explicit finish и onLimit могут оставить ещё не запущенные ветки.
			// Для Pending нет и не должно быть visit_state. Starting появляется
			// при durable reserve до любого thread/turn и также не обещает
			// отдельного state-события: после crash его уже некому дописать.
			if visit.State == scheduler.Pending || visit.State == scheduler.Starting {
				continue
			}
			event, ok := states[visit.VisitID]
			if !ok || event.State != string(visit.State) || event.TurnID != visit.TurnID {
				return false
			}
		}
		return true
	}
	for _, step := range snapshot.Meta.Steps {
		event, ok := states[step.ID]
		if !ok || event.State != string(step.State) || event.TurnID != step.TurnID {
			return false
		}
	}
	return true
}

func parseReadRunArguments(command string, args []string, deps dependencies) (string, string, error) {
	positionals, values, err := parseOptions(args, map[string]bool{"root": true})
	if err != nil || len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		if err != nil {
			return "", "", err
		}
		return "", "", fmt.Errorf("использование: lawa %s <run-id> [--root <путь>]", command)
	}
	root, err := resolveRoot(values["root"], deps.userHomeDir)
	return positionals[0], root, err
}

type logsArguments struct {
	runID, stepID, visitID, root string
	follow                       bool
}

func parseLogsArguments(args []string, deps dependencies) (logsArguments, error) {
	var parsed logsArguments
	filtered := make([]string, 0, len(args))
	for _, argument := range args {
		if argument == "--follow" {
			if parsed.follow {
				return logsArguments{}, errors.New("параметр --follow повторён")
			}
			parsed.follow = true
			continue
		}
		filtered = append(filtered, argument)
	}
	positionals, values, err := parseOptions(filtered, map[string]bool{"root": true, "visit": true})
	if err != nil || len(positionals) < 1 || len(positionals) > 2 || strings.TrimSpace(positionals[0]) == "" {
		if err != nil {
			return logsArguments{}, err
		}
		return logsArguments{}, errors.New("использование: lawa logs <run-id> [step-id] [--visit <visit-id>] [--root <путь>] [--follow]")
	}
	parsed.runID = positionals[0]
	if len(positionals) == 2 {
		parsed.stepID = positionals[1]
		if strings.TrimSpace(parsed.stepID) == "" {
			return logsArguments{}, errors.New("step-id должен быть непустым")
		}
	}
	if visitID, exists := values["visit"]; exists {
		if strings.TrimSpace(visitID) == "" {
			return logsArguments{}, errors.New("visit-id должен быть непустым")
		}
		parsed.visitID = visitID
	}
	if parsed.stepID != "" && parsed.visitID != "" {
		return logsArguments{}, errors.New("step-id и --visit взаимоисключающие")
	}
	parsed.root, err = resolveRoot(values["root"], deps.userHomeDir)
	return parsed, err
}

// validateLogsFilter проверяет фильтр по неизменяемому workflow/append-only
// history до чтения журнала. StepID v2 может быть известен ещё до первого visit;
// VisitID, напротив, обязан уже существовать в durable metadata.
func validateLogsFilter(snapshot runstore.Snapshot, parsed logsArguments) error {
	if parsed.visitID != "" {
		if snapshot.Meta.Version != 4 {
			return errors.New("--visit поддерживается только для workflow version=2")
		}
		for _, visit := range snapshot.Meta.Visits {
			if visit.VisitID == parsed.visitID {
				return nil
			}
		}
		return fmt.Errorf("не содержит посещение %q", parsed.visitID)
	}
	if parsed.stepID == "" {
		return nil
	}
	if snapshot.Meta.Version == 4 {
		for _, step := range snapshot.Workflow.Steps {
			if step.ID == parsed.stepID {
				return nil
			}
		}
	} else {
		for _, step := range snapshot.Meta.Steps {
			if step.ID == parsed.stepID {
				return nil
			}
		}
	}
	return fmt.Errorf("не содержит шаг %q", parsed.stepID)
}

func runTerminal(snapshot runstore.Snapshot) bool {
	if snapshot.Meta.Version == 4 {
		return snapshot.Meta.RunState != runstore.RunRunning
	}
	for _, step := range snapshot.Meta.Steps {
		switch step.State {
		case scheduler.Pending, scheduler.Starting, scheduler.Running, scheduler.Unknown, scheduler.WaitingForApproval:
			return false
		}
	}
	return len(snapshot.Meta.Steps) != 0
}
