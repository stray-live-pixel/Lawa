package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/appdriver"
	"github.com/stray-live-pixel/Lawa/internal/coordinator"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// appRunCommand создаёт только устойчивое состояние run. Жизненным циклом задач
// владеет вызывающий чат Codex App через app-next/app-claim/app-reset-claim/app-bind/app-update; поэтому
// здесь нет проверки и запуска отдельного app-server. Повторяющиеся серии пока
// остаются у legacy run: их фоновый процесс не имеет app-инструментов Desktop.
func appRunCommand(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	parsed, err := parseRunArguments(args)
	if err != nil {
		return err
	}
	if parsed.executable != "" || parsed.repeat != "" {
		return errors.New("app-run не поддерживает --codex и --repeat; этими режимами владеет отдельный legacy app-server")
	}
	if parsed.root, err = resolveRoot(parsed.root, deps.userHomeDir); err != nil {
		return err
	}
	if parsed.cwd, err = filepath.Abs(parsed.cwd); err != nil {
		return fmt.Errorf("определить cwd: %w", err)
	}
	workflowJSON, err := os.ReadFile(parsed.workflow)
	if err != nil {
		return fmt.Errorf("открыть workflow: %w", err)
	}
	definition, err := workflow.Decode(strings.NewReader(string(workflowJSON)))
	if err != nil {
		return fmt.Errorf("проверить %q: %w", parsed.workflow, err)
	}
	// send_message_to_thread принимает model и thinking, но не service tier.
	// Молчаливое игнорирование speed изменило бы оплаченный режим пользователя.
	for _, step := range definition.Steps {
		if step.Speed != nil {
			return fmt.Errorf("app-run: шаг %q задаёт speed, но Codex App task API пока не принимает service tier; удалите speed или используйте legacy run", step.ID)
		}
	}
	if parsed.taskFile != "" {
		if parsed.task, err = readTextArgument(parsed.taskFile, "постановку"); err != nil {
			return err
		}
	}
	if parsed.commentFile != "" {
		if parsed.comment, err = readTextArgument(parsed.commentFile, "комментарий"); err != nil {
			return err
		}
	}
	if !utf8.ValidString(parsed.task+parsed.comment+parsed.initiator) || strings.TrimSpace(parsed.task) == "" || strings.TrimSpace(parsed.initiator) == "" {
		return errors.New("постановка и ID чата должны быть непустым текстом UTF-8; комментарий также должен быть UTF-8")
	}
	snapshot, err := runstore.Create(parsed.root, runstore.Input{
		WorkflowJSON: workflowJSON, Task: parsed.task, Comment: parsed.comment, CWD: parsed.cwd,
		InitiatorThreadID: parsed.initiator, ParentRunID: parsed.parentRun,
	})
	if err != nil {
		return fmt.Errorf("создать app-native запуск: %w", err)
	}
	if err = refreshAppArtifacts(ctx, parsed.root, snapshot.Meta.RunID, deps); err != nil {
		return err
	}
	return writeJSON(out, struct {
		RunID string `json:"runId"`
	}{snapshot.Meta.RunID})
}

func appNextCommand(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	runID, root, err := parseAppRunReference("app-next", args, deps)
	if err != nil {
		return err
	}
	action, err := appdriver.Next(root, runID)
	if err != nil {
		return err
	}
	if err = refreshAppArtifacts(ctx, root, runID, deps); err != nil {
		return err
	}
	return writeJSON(out, action)
}

func appClaimCommand(args []string, out io.Writer, deps dependencies) error {
	runID, root, values, err := parseAppMutation("app-claim", args, deps, map[string]bool{"step": true})
	if err != nil {
		return err
	}
	if strings.TrimSpace(values["step"]) == "" {
		return errors.New("app-claim требует --step")
	}
	claimed, err := appdriver.Claim(root, runID, values["step"])
	if err != nil {
		return err
	}
	return writeJSON(out, struct {
		MayCreate bool `json:"mayCreate"`
	}{claimed})
}

func appResetClaimCommand(args []string, out io.Writer, deps dependencies) error {
	runID, root, values, err := parseAppMutation("app-reset-claim", args, deps, map[string]bool{
		"step": true, "confirm-reset": true,
	})
	if err != nil {
		return err
	}
	stepID := strings.TrimSpace(values["step"])
	if stepID == "" || values["confirm-reset"] != stepID {
		return errors.New("app-reset-claim требует --step <id> и точное --confirm-reset <id>; повтор может создать дубликат задачи")
	}
	if err = appdriver.ResetClaim(root, runID, stepID); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "ok")
	return err
}

func appBindCommand(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	runID, root, values, err := parseAppMutation("app-bind", args, deps, map[string]bool{"step": true, "thread-id": true})
	if err != nil {
		return err
	}
	if strings.TrimSpace(values["step"]) == "" || strings.TrimSpace(values["thread-id"]) == "" {
		return errors.New("app-bind требует --step и --thread-id")
	}
	if err = appdriver.Bind(root, runID, values["step"], values["thread-id"]); err != nil {
		return err
	}
	if err = refreshAppArtifacts(ctx, root, runID, deps); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "ok")
	return err
}

func appUpdateCommand(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	runID, root, values, err := parseAppMutation("app-update", args, deps, map[string]bool{
		"step": true, "state": true, "revision": true, "result-file": true,
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(values["step"]) == "" || strings.TrimSpace(values["state"]) == "" || strings.TrimSpace(values["revision"]) == "" {
		return errors.New("app-update требует --step, --state и --revision")
	}
	revision, err := strconv.ParseUint(values["revision"], 10, 64)
	if err != nil {
		return fmt.Errorf("app-update: неверная --revision: %w", err)
	}
	var result []byte
	if path := values["result-file"]; path != "" {
		if result, err = os.ReadFile(path); err != nil {
			return fmt.Errorf("прочитать финальный ответ: %w", err)
		}
	}
	if err = appdriver.Update(root, runID, values["step"], values["state"], revision, result); err != nil {
		return err
	}
	if err = refreshAppArtifacts(ctx, root, runID, deps); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "ok")
	return err
}

func parseAppRunReference(command string, args []string, deps dependencies) (string, string, error) {
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

func parseAppMutation(command string, args []string, deps dependencies, extra map[string]bool) (string, string, map[string]string, error) {
	allowed := map[string]bool{"root": true}
	for name := range extra {
		allowed[name] = true
	}
	positionals, values, err := parseOptions(args, allowed)
	if err != nil || len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		if err != nil {
			return "", "", nil, err
		}
		return "", "", nil, fmt.Errorf("использование: lawa %s <run-id> --step <id> [параметры]", command)
	}
	root, err := resolveRoot(values["root"], deps.userHomeDir)
	return positionals[0], root, values, err
}

func writeJSON(out io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}

// refreshAppArtifacts открывает run после завершённой мутации и строит отчёты из
// того же Metadata. statusPublisher скрывает ошибку необязательного PlantUML в
// Markdown, а io.Discard не смешивает чат-сводку с JSON протокола app-next.
func refreshAppArtifacts(ctx context.Context, root, runID string, deps dependencies) (err error) {
	run, err := runstore.OpenLocked(root, runID)
	if err != nil {
		return fmt.Errorf("обновить app-native отчёт: %w", err)
	}
	defer func() { err = errors.Join(err, run.Close()) }()
	status, err := coordinator.CurrentStatus(run)
	if err != nil {
		return err
	}
	publisher := newStatusPublisher(ctx, io.Discard, filepath.Join(root, runID), deps.renderer, deps.chatInterval, deps.now)
	return publisher.Publish(status)
}
