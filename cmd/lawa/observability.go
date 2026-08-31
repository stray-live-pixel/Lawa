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
	if _, err = fmt.Fprintf(out, "runId: %s\nworkflow: %s\nruntime: %s\ncwd: %s\n", runID, snapshot.Workflow.ID, runtime, snapshot.Meta.CWD); err != nil {
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
		if _, err = fmt.Fprintf(out, "\n%s: %s\n  thread: %s\n  turn: %s\n  процесс: %s\n  активность: %s\n", step.ID, step.State, threadID, turnID, process, activity); err != nil {
			return err
		}
		if summary.Message != "" {
			if _, err = fmt.Fprintf(out, "  сообщение: %s\n", summary.Message); err != nil {
				return err
			}
		}
	}
	if snapshot.HistoricalAppNative {
		_, err = fmt.Fprintln(out, "\nЭтот run не возобновляется: его задачи могли быть созданы через Codex Desktop.")
	}
	return err
}

// logsCommand печатает только нормализованные события Lawa. `--follow` перечитывает
// журнал коротким polling без открытия App Server и завершается после терминала.
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
	printed := 0
	for {
		events, readErr := runstore.ReadEvents(root, runID)
		if readErr != nil {
			return fmt.Errorf("прочитать события run %q: %w", runID, readErr)
		}
		for ; printed < len(events); printed++ {
			event := events[printed]
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
		if runTerminal(snapshot) || snapshot.HistoricalAppNative {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
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
