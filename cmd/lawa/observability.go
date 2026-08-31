package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
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
	for _, step := range snapshot.Meta.Steps {
		summary := summaries[step.ID]
		threadID, turnID := step.CodexThreadID, step.TurnID
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
		if _, err = fmt.Fprintf(out, "\n%s: %s\n  thread: %s\n  turn: %s\n  процесс: %s\n  активность: %s\n",
			runstore.SafeTerminalText(step.ID), runstore.SafeTerminalText(string(step.State)),
			runstore.SafeTerminalText(threadID), runstore.SafeTerminalText(turnID),
			runstore.SafeTerminalText(process), runstore.SafeTerminalText(activity)); err != nil {
			return err
		}
		if summary.Message != "" {
			if _, err = fmt.Fprintf(out, "  сообщение: %s\n", runstore.SafeTerminalText(summary.Message)); err != nil {
				return err
			}
		}
		if len(summary.ActiveItemTypes) != 0 {
			if _, err = fmt.Fprintf(out, "  действие: %s\n", runstore.SafeTerminalText(strings.Join(summary.ActiveItemTypes, ", "))); err != nil {
				return err
			}
		}
	}
	if snapshot.HistoricalAppNative {
		_, err = fmt.Fprintln(out, "\nЭтот run не возобновляется: его задачи могли быть созданы через Codex Desktop.")
	}
	return err
}

// logsCommand печатает только нормализованные события Lawa. `--follow` читает
// новые полные строки с сохранённой byte-позиции и не открывает App Server.
// Терминальный meta.json сам по себе недостаточен для выхода: coordinator пишет
// его раньше финального step_state, который иначе мог бы потеряться для оператора.
func logsCommand(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	runID, stepID, root, follow, err := parseLogsArguments(args, deps)
	if err != nil {
		return err
	}
	if stepID != "" {
		snapshot, loadErr := runstore.LoadForDashboard(root, runID)
		if loadErr != nil {
			return fmt.Errorf("прочитать run %q: %w", runID, loadErr)
		}
		known := false
		for _, step := range snapshot.Meta.Steps {
			known = known || step.ID == stepID
		}
		if !known {
			return fmt.Errorf("run %q не содержит шаг %q", runID, stepID)
		}
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
		if follow {
			events, offset, readErr = runstore.ReadEventsAfter(root, runID, offset)
		} else {
			events, readErr = runstore.ReadEvents(root, runID)
		}
		if readErr != nil {
			return fmt.Errorf("прочитать события run %q: %w", runID, readErr)
		}
		for _, event := range events {
			if event.Kind == "step_state" || event.Kind == "thread_reconciled" {
				finalStates[event.StepID] = eventState{State: event.State, TurnID: event.TurnID}
			}
			if stepID != "" && event.StepID != stepID {
				continue
			}
			if _, err = fmt.Fprintln(out, runstore.FormatEvent(event)); err != nil {
				return err
			}
		}
		if !follow {
			return nil
		}
		snapshot, loadErr := runstore.LoadForDashboard(root, runID)
		if loadErr != nil {
			return fmt.Errorf("прочитать run %q: %w", runID, loadErr)
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

// terminalEventsRecorded подтверждает, что журнал дошёл до состояния и turn из
// meta.json каждого кубика. Сравнение turn не даёт старому succeeded-событию
// преждевременно завершить follow после ручного продолжения того же thread.
func terminalEventsRecorded(snapshot runstore.Snapshot, states map[string]eventState) bool {
	// В старых форматах обязательного events.jsonl ещё не было. Их сохранённый
	// терминал остаётся читаемым и не превращает `--follow` в вечное ожидание.
	if snapshot.Meta.Version < 3 {
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

func parseLogsArguments(args []string, deps dependencies) (runID, stepID, root string, follow bool, err error) {
	filtered := make([]string, 0, len(args))
	for _, argument := range args {
		if argument == "--follow" {
			if follow {
				return "", "", "", false, errors.New("параметр --follow повторён")
			}
			follow = true
			continue
		}
		filtered = append(filtered, argument)
	}
	positionals, values, err := parseOptions(filtered, map[string]bool{"root": true})
	if err != nil || len(positionals) < 1 || len(positionals) > 2 || strings.TrimSpace(positionals[0]) == "" {
		if err != nil {
			return "", "", "", false, err
		}
		return "", "", "", false, errors.New("использование: lawa logs <run-id> [step-id] [--root <путь>] [--follow]")
	}
	if len(positionals) == 2 {
		stepID = positionals[1]
		if strings.TrimSpace(stepID) == "" {
			return "", "", "", false, errors.New("step-id должен быть непустым")
		}
	}
	root, err = resolveRoot(values["root"], deps.userHomeDir)
	return positionals[0], stepID, root, follow, err
}

func runTerminal(snapshot runstore.Snapshot) bool {
	for _, step := range snapshot.Meta.Steps {
		switch step.State {
		case scheduler.Pending, scheduler.Starting, scheduler.Running, scheduler.Unknown, scheduler.WaitingForApproval:
			return false
		}
	}
	return len(snapshot.Meta.Steps) != 0
}
