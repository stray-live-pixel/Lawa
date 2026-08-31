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
	"time"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/appdriver"
	"github.com/stray-live-pixel/Lawa/internal/coordinator"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/series"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// appRunCommand создаёт только устойчивое состояние run. Жизненным циклом задач
// владеет вызывающий чат Codex App через app-next, creation/continuation claims,
// app-bind и app-update; поэтому
// здесь нет проверки и запуска отдельного app-server. Для повторяющейся серии
// команда дополнительно сохраняет неизменяемый шаблон и возвращает первый
// app-series action; дальнейшие heartbeat-вызовы app-series-next не зависят от
// времени жизни terminal-сеанса исходной задачи.
func appRunCommand(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	parsed, err := parseRunArguments(args)
	if err != nil {
		return err
	}
	if parsed.executable != "" {
		return errors.New("app-run не поддерживает --codex: задачами владеет Codex App")
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
	input := runstore.Input{
		WorkflowJSON: workflowJSON, Task: parsed.task, Comment: parsed.comment, CWD: parsed.cwd,
		InitiatorThreadID: parsed.initiator, ParentRunID: parsed.parentRun,
	}
	if parsed.repeat != "" {
		config, schedule, parseErr := series.ParseConfig(parsed.repeat, parsed.repeatDelay, parsed.cron, parsed.timezone, parsed.maxRuns)
		if parseErr != nil {
			return parseErr
		}
		owner, createErr := series.CreateApp(parsed.root, config, appTemplate(input))
		if createErr != nil {
			return fmt.Errorf("создать app-native серию: %w", createErr)
		}
		defer owner.Close()
		seriesID := owner.Snapshot().SeriesID
		action, advanceErr := advanceAppSeries(owner, parsed.root, input, schedule, deps.now())
		if advanceErr != nil {
			return fmt.Errorf("продвинуть app-native серию %s: %w", seriesID, advanceErr)
		}
		if action.RunID != "" {
			if advanceErr = refreshAppArtifacts(ctx, parsed.root, action.RunID, deps); advanceErr != nil {
				return fmt.Errorf("app-native серия %s, run %s: %w", seriesID, action.RunID, advanceErr)
			}
		}
		return writeJSON(out, action)
	}
	snapshot, err := runstore.Create(parsed.root, input)
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

// appSeriesAction — устойчивое решение одного короткого прохода серии. run
// означает, что управляющий turn должен продолжить ровно сохранённый runId через
// app-next. wait содержит абсолютную следующую точку и разрешает turn завершиться:
// heartbeat позже вызовет app-series-next. complete/stopped терминальны.
type appSeriesAction struct {
	Kind      string       `json:"kind"`
	SeriesID  string       `json:"seriesId"`
	RunID     string       `json:"runId,omitempty"`
	NextRunAt *time.Time   `json:"nextRunAt,omitempty"`
	State     series.State `json:"state"`
}

func appSeriesNextCommand(ctx context.Context, args []string, out io.Writer, deps dependencies) error {
	seriesID, root, err := parseAppRunReference("app-series-next", args, deps)
	if err != nil {
		return err
	}
	template, err := series.LoadAppTemplate(root, seriesID)
	if err != nil {
		return fmt.Errorf("прочитать app-native серию: %w", err)
	}
	owner, err := series.Open(root, seriesID)
	if err != nil {
		return fmt.Errorf("открыть app-native серию: %w", err)
	}
	defer owner.Close()
	config := owner.Snapshot().Config
	maxRuns := ""
	if config.MaxRuns != 0 {
		maxRuns = strconv.Itoa(config.MaxRuns)
	}
	_, schedule, err := series.ParseConfig(string(config.Mode), config.Delay, config.Cron, config.TimeZone, maxRuns)
	if err != nil {
		return fmt.Errorf("прочитать расписание app-native серии: %w", err)
	}
	action, err := advanceAppSeries(owner, root, appInput(template), schedule, deps.now())
	if err != nil {
		return err
	}
	if action.RunID != "" {
		if err = refreshAppArtifacts(ctx, root, action.RunID, deps); err != nil {
			return err
		}
	}
	return writeJSON(out, action)
}

// appSeriesFailCommand фиксирует terminal failure только по точной паре
// seriesId/runId, которую управляющий чат только что наблюдал. Отдельная команда
// не позволяет app-series-next угадать, является ли interrupted следствием
// явной остановки пользователя или требует продолжения той же задачи.
func appSeriesFailCommand(args []string, out io.Writer, deps dependencies) error {
	positionals, values, err := parseOptions(args, map[string]bool{"root": true, "run": true, "reason-file": true})
	if err != nil || len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" || strings.TrimSpace(values["run"]) == "" || strings.TrimSpace(values["reason-file"]) == "" {
		if err != nil {
			return err
		}
		return errors.New("использование: lawa app-series-fail <series-id> --run <run-id> --reason-file <путь> [--root <путь>]")
	}
	root, err := resolveRoot(values["root"], deps.userHomeDir)
	if err != nil {
		return err
	}
	reason, err := readTextArgument(values["reason-file"], "причину остановки серии")
	if err != nil || strings.TrimSpace(reason) == "" {
		if err == nil {
			err = errors.New("причина остановки серии должна быть непустым текстом UTF-8")
		}
		return err
	}
	owner, err := series.Open(root, positionals[0])
	if err != nil {
		return err
	}
	defer owner.Close()
	meta := owner.Snapshot()
	if meta.Driver != series.AppDriver || meta.CurrentRunID != values["run"] {
		return fmt.Errorf("app-series-fail: текущий run серии %q равен %q, а не %q", meta.SeriesID, meta.CurrentRunID, values["run"])
	}
	run, err := runstore.Load(root, meta.CurrentRunID)
	if err != nil {
		return fmt.Errorf("app-series-fail: прочитать текущий run: %w", err)
	}
	terminalFailure := false
	for _, step := range run.Meta.Steps {
		terminalFailure = terminalFailure || step.State == scheduler.Failed || step.State == scheduler.Cancelled
	}
	if !terminalFailure {
		return errors.New("app-series-fail: текущий run не содержит failed или interrupted кубика")
	}
	if err = owner.FinishRun(errors.New(reason)); err != nil {
		return err
	}
	return writeJSON(out, appSeriesAction{Kind: "failed", SeriesID: meta.SeriesID, State: series.Failed})
}

// advanceAppSeries сводит повторный heartbeat к одной атомарной операции. Уже
// существующий незавершённый run всегда возвращается как есть. Только полностью
// успешный run освобождает слот; следующий создаётся не раньше сохранённой точки,
// поэтому конкурентные или запоздавшие heartbeat не порождают дубликаты.
func advanceAppSeries(owner *series.LockedSeries, root string, input runstore.Input, schedule series.Schedule, now time.Time) (appSeriesAction, error) {
	if owner == nil || schedule == nil {
		return appSeriesAction{}, errors.New("app-native серия требует владельца и проверенное расписание")
	}
	meta := owner.Snapshot()
	base := appSeriesAction{SeriesID: meta.SeriesID, State: meta.State}
	if meta.Driver != series.AppDriver {
		return base, errors.New("серия не принадлежит app-native driver")
	}
	if meta.State == series.Completed || meta.State == series.Stopped || meta.State == series.Failed {
		base.Kind = string(meta.State)
		if meta.State == series.Completed {
			base.Kind = "complete"
		}
		return base, nil
	}
	if meta.CurrentRunID != "" {
		run, err := runstore.Load(root, meta.CurrentRunID)
		if err != nil {
			return base, fmt.Errorf("прочитать текущий run серии %q: %w", meta.SeriesID, err)
		}
		complete := len(run.Meta.Steps) != 0
		for _, step := range run.Meta.Steps {
			complete = complete && step.State == scheduler.Succeeded
		}
		if !complete {
			base.Kind, base.RunID, base.State = "run", meta.CurrentRunID, series.Running
			return base, nil
		}
		if err = owner.FinishRun(nil); err != nil {
			return base, fmt.Errorf("завершить успешный run app-серии: %w", err)
		}
		meta = owner.Snapshot()
	}
	stopped, err := owner.StopRequested()
	if err != nil {
		return base, err
	}
	if stopped {
		if err = owner.FinishSeries(series.Stopped); err != nil {
			return base, err
		}
		return appSeriesAction{Kind: "stopped", SeriesID: meta.SeriesID, State: series.Stopped}, nil
	}
	if meta.Config.MaxRuns != 0 && meta.RunsStarted >= meta.Config.MaxRuns {
		if err = owner.FinishSeries(series.Completed); err != nil {
			return base, err
		}
		return appSeriesAction{Kind: "complete", SeriesID: meta.SeriesID, State: series.Completed}, nil
	}
	if meta.NextRunAt == nil {
		next := schedule.Next(now, meta.RunsStarted)
		if err = owner.SetNext(next); err != nil {
			return base, fmt.Errorf("сохранить следующую точку app-серии: %w", err)
		}
		meta = owner.Snapshot()
	}
	if now.Before(*meta.NextRunAt) {
		return appSeriesAction{Kind: "wait", SeriesID: meta.SeriesID, NextRunAt: meta.NextRunAt, State: series.Waiting}, nil
	}
	var runID string
	started, err := owner.StartRun(func() (string, error) {
		snapshot, createErr := runstore.Create(root, input)
		if createErr == nil {
			runID = snapshot.Meta.RunID
		}
		return runID, createErr
	}, func(created string) error {
		return runstore.RemoveUnstarted(root, created)
	})
	if err != nil {
		return base, fmt.Errorf("создать run app-серии: %w", err)
	}
	if !started {
		if err = owner.FinishSeries(series.Stopped); err != nil {
			return base, err
		}
		return appSeriesAction{Kind: "stopped", SeriesID: meta.SeriesID, State: series.Stopped}, nil
	}
	return appSeriesAction{Kind: "run", SeriesID: meta.SeriesID, RunID: runID, State: series.Running}, nil
}

func appTemplate(input runstore.Input) series.AppTemplate {
	return series.AppTemplate{
		WorkflowJSON: string(input.WorkflowJSON), Task: input.Task, Comment: input.Comment, CWD: input.CWD,
		InitiatorThreadID: input.InitiatorThreadID, ParentRunID: input.ParentRunID,
	}
}

func appInput(template series.AppTemplate) runstore.Input {
	return runstore.Input{
		WorkflowJSON: []byte(template.WorkflowJSON), Task: template.Task, Comment: template.Comment, CWD: template.CWD,
		InitiatorThreadID: template.InitiatorThreadID, ParentRunID: template.ParentRunID,
	}
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

func appContinueClaimCommand(args []string, out io.Writer, deps dependencies) error {
	runID, root, values, err := parseAppMutation("app-continue-claim", args, deps, map[string]bool{"step": true, "turn-id": true})
	if err != nil {
		return err
	}
	if strings.TrimSpace(values["step"]) == "" || strings.TrimSpace(values["turn-id"]) == "" {
		return errors.New("app-continue-claim требует --step и --turn-id interrupted turn")
	}
	claimed, err := appdriver.ClaimContinuation(root, runID, values["step"], values["turn-id"])
	if err != nil {
		return err
	}
	return writeJSON(out, struct {
		MayContinue bool `json:"mayContinue"`
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
