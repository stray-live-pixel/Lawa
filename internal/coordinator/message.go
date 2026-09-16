package coordinator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/stray-live-pixel/Lawa/internal/capacity"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// reconcileOrderMessage восстанавливает только доказанную доставку. Если после
// сохранения намерения turn/start не вернул ID, старый terminal turn не разрешает
// повтор: сообщение могло быть отправлено. Новый ID подтверждает приём сервером.
func reconcileOrderMessage(run *runstore.LockedRun, snapshot runstore.Snapshot, observer Observer) error {
	if snapshot.Meta.Order == nil || !snapshot.Meta.Order.Pending {
		return nil
	}
	step := snapshot.Meta.Steps[0]
	observed, err := observer.Inspect(step.CodexThreadID)
	if err != nil {
		return err
	}
	if observed.LatestTurnID == "" || observed.LatestTurnID == step.TurnID {
		return errors.New("доставка сообщения Чела неоднозначна; проверьте историю чата Codex, автоматическая повторная отправка запрещена")
	}
	return run.SetTurn(step.ID, observed.LatestTurnID)
}

// Message продолжает того же Босса явной репликой Чела после terminal-ответа.
// Это не автоматический retry. Используются штатные callbacks, журнал, capacity
// и адресный interrupt координатора; история чата остаётся у Codex. Run должен
// быть заблокирован вызывающим кодом до конца операции.
func Message(ctx context.Context, run *runstore.LockedRun, options Options, text string) (err error) {
	snapshot, err := run.Load()
	if err != nil {
		return err
	}
	if snapshot.Meta.Order == nil {
		return errors.New("reply принимает только заказ, созданный через lawa order")
	}
	observer := &sharedObserver{ctx: ctx, client: options.Client, cwd: snapshot.Meta.CWD}
	defer func() { err = errors.Join(err, observer.Close()) }()
	if err = reconcileOrderMessage(run, snapshot, observer); err != nil {
		return err
	}
	step := snapshot.Meta.Steps[0]
	observed, err := observer.Inspect(step.CodexThreadID)
	if err != nil {
		return err
	}
	status, err := observed.Status()
	if err != nil {
		return err
	}
	state, err := stateFromObservation(status)
	if err != nil {
		return err
	}
	if observed.LatestTurnID == "" {
		return errors.New("нет подтверждённого turn Босса")
	}
	if err = run.SetTurn(step.ID, observed.LatestTurnID); err != nil {
		return err
	}
	if err = run.Update(step.ID, state, step.CodexThreadID); err != nil {
		return err
	}
	if options.Capacity == nil {
		options.Capacity = capacity.Unlimited()
	}
	lease, available, err := options.Capacity.TryAcquire()
	if err != nil {
		return err
	}
	if !available {
		return errors.New("нет свободного слота для ответа Босса; повторите reply после завершения активной работы")
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	runDir, err := run.ResolveDirectory(options.Root)
	if err != nil {
		return err
	}
	command := codex.Command{
		CWD: snapshot.Meta.CWD, Text: "Сообщение Чела по текущему заказу (дословно):\n" + text,
		Permissions: characterPermissions(snapshot.Workflow, snapshot.Workflow.Steps[0], runDir, filepath.Join(runDir, "memory", step.ThreadID+".md"), step.ThreadID),
	}
	applyRuntimeSettings(&command, snapshot.Workflow.Model, snapshot.Workflow.Steps[0])
	if options.ConfigureCommand != nil {
		options.ConfigureCommand(snapshot, &command)
	}
	if err = run.ReserveMessage(text); err != nil {
		return err
	}
	turnCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	execution := newActiveExecution(cancel, step.CodexThreadID, lease)
	active := map[string]*activeExecution{step.ID: execution}
	results := make(chan launchResult, 1)
	startContinuation(run, options.Client, turnCtx, Continuation{StepID: step.ID, ThreadID: step.CodexThreadID, Command: command}, execution, results)
	select {
	case result := <-results:
		err = errors.Join(finishExecution(active, step.ID), saveLaunchResult(run, result))
		// Ошибка thread/resume или локальной подготовки ещё не отправляла
		// реплику. Сохраняем возможность явного повтора после устранения причины.
		if result.err != nil && !result.result.TurnAttempted && result.result.TurnID == "" {
			err = errors.Join(err, run.ReleaseUnsentMessage())
		}
		if err == nil && result.result.Status != "completed" {
			err = fmt.Errorf("ответ Босса: %s", result.result.Status)
		}
		return err
	case <-ctx.Done():
		return interruptActive(ctx.Err(), run, active, results)
	}
}
