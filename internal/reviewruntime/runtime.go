// Package reviewruntime исполняет три независимых этапа review через Codex.
// Сохранённые структуры и артефакты являются контрактом этапов, а текст ответа
// модели сам по себе никогда не считается успешным результатом.
package reviewruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// Engine использует отдельный app-server для каждого этапа и каждой попытки.
// RunCodex подменяется в проверках; nil использует настоящий Codex.
type Engine struct {
	Store      *reviewstore.Store
	Executable string
	RunCodex   func(context.Context, codex.Command) (codex.Result, error)
}

// Run начинает только новый review. Retry явно продолжает неуспешный этап.
func (e *Engine) Run(ctx context.Context, id string) error { return e.execute(ctx, id, false) }

// Retry сохраняет успешные этапы и все предыдущие попытки. Неоднозначная внешняя
// публикация сверяется агентом с сохранёнными receipts, а не повторяется вслепую.
func (e *Engine) Retry(ctx context.Context, id string) error { return e.execute(ctx, id, true) }

// execute удерживает межпроцессную блокировку всё время работы, включая отмену.
func (e *Engine) execute(ctx context.Context, id string, retry bool) error {
	if e.Store == nil {
		return errors.New("reviewruntime: не задано хранилище")
	}
	lock, err := e.Store.AcquireExecution(id)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, err := e.Store.RecoverInterrupted(id); err != nil {
		return err
	}
	r, err := e.Store.Load(id)
	if err != nil {
		return err
	}
	local := *e
	e = &local
	if e.Executable == "" {
		e.Executable = r.Config.CodexExecutable
	}
	if r.StopRequested && !retry {
		_, err = e.Store.Update(id, func(r *reviewstore.Review) error { r.State = reviewstore.Stopped; return nil })
		return errors.Join(errors.New("ревью остановлено до запуска"), err)
	}
	if r.State == reviewstore.Succeeded {
		return errors.New("ревью уже завершено")
	}
	if !retry && r.State != reviewstore.Pending {
		return errors.New("для продолжения используйте явный повтор")
	}
	stopped := false
	_, err = e.Store.Update(id, func(r *reviewstore.Review) error {
		if r.StopRequested && !retry {
			r.State = reviewstore.Stopped
			stopped = true
			return nil
		}
		if retry {
			r.StopRequested = false
		}
		r.Error = ""
		r.State = reviewstore.Running
		return nil
	})
	if err != nil {
		return err
	}
	if stopped {
		return context.Canceled
	}
	if e.RunCodex == nil {
		if err = e.validateModels(ctx, r); err != nil {
			_, saveErr := e.Store.Update(id, func(r *reviewstore.Review) error { r.State = reviewstore.Failed; r.Error = err.Error(); return nil })
			return errors.Join(err, saveErr)
		}
	}
	runctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-runctx.Done():
				return
			case <-ticker.C:
				r, err := e.Store.Load(id)
				if err != nil || r.StopRequested {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-watchDone }()
	for _, stage := range []reviewstore.StageID{reviewstore.ContextStage, reviewstore.ReviewStage, reviewstore.PresentationStage} {
		r, err = e.Store.Load(id)
		if err != nil {
			return err
		}
		if stageState(r, stage) == reviewstore.Succeeded {
			continue
		}
		if r.StopRequested {
			cancel()
		}
		if err = e.runStage(runctx, id, stage); err != nil {
			state := reviewstore.Failed
			if runctx.Err() != nil || errors.Is(err, context.Canceled) {
				state = reviewstore.Stopped
			}
			_, saveErr := e.Store.Update(id, func(r *reviewstore.Review) error { r.State = state; r.Error = err.Error(); return nil })
			return errors.Join(err, saveErr)
		}
	}
	_, err = e.Store.Update(id, func(r *reviewstore.Review) error { r.State = reviewstore.Succeeded; r.Error = ""; return nil })
	return err
}

// stageState не полагается на порядок сериализованного массива.
func stageState(r reviewstore.Review, id reviewstore.StageID) reviewstore.State {
	for _, s := range r.Stages {
		if s.ID == id {
			return s.State
		}
	}
	return reviewstore.Pending
}

// mutateStage обновляет текущую попытку под блокировкой хранилища.
func (e *Engine) mutateStage(id string, stage reviewstore.StageID, fn func(*reviewstore.Review, *reviewstore.Stage, *reviewstore.Attempt) error) error {
	_, err := e.Store.Update(id, func(r *reviewstore.Review) error {
		for i := range r.Stages {
			if r.Stages[i].ID == stage {
				s := &r.Stages[i]
				if len(s.Attempts) == 0 {
					return errors.New("попытка ещё не создана")
				}
				return fn(r, s, &s.Attempts[len(s.Attempts)-1])
			}
		}
		return errors.New("этап не найден")
	})
	return err
}

// runStage выдаёт агенту узкие инструменты записи и требует явного завершения
// контракта. Callback-и app-server синхронны; mutex защищает interrupt от отмены.
func (e *Engine) runStage(ctx context.Context, id string, stage reviewstore.StageID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r, err := e.Store.Load(id)
	if err != nil {
		return err
	}
	number := 1
	for _, s := range r.Stages {
		if s.ID == stage {
			number = len(s.Attempts) + 1
		}
	}
	dir := filepath.Join(e.Store.Root, id)
	scratch := filepath.Join(dir, "scratch", string(stage), fmt.Sprint(number))
	if err = os.MkdirAll(scratch, 0700); err != nil {
		return err
	}
	prompt := stagePrompt(r, stage, dir, scratch)
	path := fmt.Sprintf("artifacts/attempts/%s/%d/prompt.md", stage, number)
	if err = e.Store.SaveArtifact(id, path, []byte(prompt)); err != nil {
		return err
	}
	_, err = e.Store.Update(id, func(r *reviewstore.Review) error {
		if r.StopRequested {
			return context.Canceled
		}
		r.CurrentStage = stage
		for i := range r.Stages {
			if r.Stages[i].ID == stage {
				s := &r.Stages[i]
				s.State = reviewstore.Running
				s.Attempts = append(s.Attempts, reviewstore.Attempt{Number: number, State: reviewstore.Running, StartedAt: time.Now().UTC(), PromptPath: path})
				return nil
			}
		}
		return errors.New("отсутствует этап review")
	})
	if err != nil {
		return err
	}
	session := &stageSession{engine: e, id: id, stage: stage, number: number, scratch: scratch}
	if stage == reviewstore.PresentationStage && r.Presentation.MarkdownPath != "" && len(r.Publications) > 0 {
		session.wrote = true
	}
	config := r.Config.Context
	if stage == reviewstore.ReviewStage {
		config = r.Config.Review
	}
	if stage == reviewstore.PresentationStage {
		config = r.Config.Presentation
	}
	var mu sync.Mutex
	var interrupt func(context.Context) error
	interruptDone := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(interruptDone)
		select {
		case <-finished:
			return
		case <-ctx.Done():
			mu.Lock()
			fn := interrupt
			mu.Unlock()
			if fn != nil {
				c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = fn(c)
			}
		}
	}()
	diagnostics := &diagnosticBuffer{}
	command := codex.Command{Executable: e.Executable, CWD: r.CWD, Text: prompt, Title: "Review · " + string(stage), Model: config.Model, Effort: config.Effort, Stderr: diagnostics,
		Permissions:  &codex.PermissionProfile{Name: "lawa_review", ReadPaths: []string{dir, r.CWD}, WritePaths: []string{scratch}},
		DynamicTools: session.tools(), CallDynamicTool: session.call, Notify: session.observe,
		OnThread: func(thread string) error {
			session.actorThreadID = thread
			return e.mutateStage(id, stage, func(_ *reviewstore.Review, _ *reviewstore.Stage, a *reviewstore.Attempt) error {
				a.ThreadID = thread
				return nil
			})
		},
		OnTurn: func(turn string, fn func(context.Context) error) error {
			mu.Lock()
			interrupt = fn
			mu.Unlock()
			return e.mutateStage(id, stage, func(_ *reviewstore.Review, _ *reviewstore.Stage, a *reviewstore.Attempt) error {
				a.TurnID = turn
				return nil
			})
		},
	}
	runner := e.RunCodex
	if runner == nil {
		runner = codex.Run
	}
	result, runErr := runner(ctx, command)
	close(finished)
	<-interruptDone
	session.collectChildUsage()
	if diagnostics.Len() > 0 {
		path := fmt.Sprintf("artifacts/attempts/%s/%d/stderr.log", stage, number)
		if err := e.Store.SaveArtifact(id, path, diagnostics.Bytes()); err != nil {
			runErr = errors.Join(runErr, err)
		}
		if runErr != nil {
			diagnostic := strings.TrimSpace(diagnostics.String())
			if len(diagnostic) > 4000 {
				diagnostic = diagnostic[len(diagnostic)-4000:]
			}
			runErr = fmt.Errorf("%w\nCodex: %s", runErr, diagnostic)
		}
	}
	if runErr == nil && result.Status != "completed" {
		runErr = fmt.Errorf("Codex завершил этап со статусом %s", result.Status)
		if result.TurnError != nil {
			runErr = fmt.Errorf("%w: %s", runErr, result.TurnError.Message)
		}
	}
	if runErr == nil && !session.completed {
		runErr = errors.New("агент не завершил структурированный контракт этапа")
	}
	if ctx.Err() != nil {
		runErr = errors.Join(runErr, ctx.Err())
	}
	if session.output != "" {
		out := fmt.Sprintf("artifacts/attempts/%s/%d/output.md", stage, number)
		if err = e.Store.SaveArtifact(id, out, []byte(session.output)); err != nil {
			runErr = errors.Join(runErr, err)
		} else {
			_ = e.mutateStage(id, stage, func(_ *reviewstore.Review, _ *reviewstore.Stage, a *reviewstore.Attempt) error {
				a.OutputPath = out
				return nil
			})
		}
	}
	state := reviewstore.Succeeded
	if runErr != nil {
		state = reviewstore.Failed
	}
	if ctx.Err() != nil {
		state = reviewstore.Stopped
	}
	err = e.mutateStage(id, stage, func(r *reviewstore.Review, s *reviewstore.Stage, a *reviewstore.Attempt) error {
		now := time.Now().UTC()
		a.FinishedAt = &now
		a.State = state
		s.State = state
		if runErr != nil {
			a.Error = runErr.Error()
		}
		if state == reviewstore.Succeeded && stage == reviewstore.ContextStage {
			r.Context.FrozenAt = &now
		}
		return nil
	})
	return errors.Join(runErr, err)
}

// validateModels проверяет доступность всех трёх настроек до первого turn; CLI
// имеет тот же контракт, что и селект UI, без тихой замены модели/effort.
func (e *Engine) validateModels(ctx context.Context, r reviewstore.Review) error {
	checkctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	models, err := codex.ListModels(checkctx, codex.Connection{Executable: e.Executable, CWD: r.CWD})
	if err != nil {
		return fmt.Errorf("получить модели Codex: %w", err)
	}
	for _, entry := range []struct {
		stage  string
		config reviewstore.AgentConfig
	}{{"Контекст", r.Config.Context}, {"Проверка", r.Config.Review}, {"Результат", r.Config.Presentation}} {
		found := false
		for _, m := range models {
			if m.ID != entry.config.Model && m.Model != entry.config.Model {
				continue
			}
			for _, effort := range m.SupportedReasoningEfforts {
				if effort.ReasoningEffort == entry.config.Effort {
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("этап %s: модель %s с effort %s недоступна аккаунту", entry.stage, entry.config.Model, entry.config.Effort)
		}
	}
	return nil
}

// diagnosticBuffer ограничивает память stderr; io.Writer всегда принимает весь
// вход, чтобы многословная диагностика не прерывала процесс сама по себе.
type diagnosticBuffer struct{ bytes.Buffer }

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 64*1024 - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
