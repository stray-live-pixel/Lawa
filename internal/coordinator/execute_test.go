package coordinator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// fakeClient моделирует только границу координатора. Протокол stdio отдельно
// проверяется internal/codex; здесь важны волны, сохранение ID и ручной повтор.
type fakeClient struct {
	mu                         sync.Mutex
	runStatuses                map[string][]string
	beforeCreateErrors         map[string][]error
	continueStatuses           map[string][]string
	inspectStatuses            map[string][]codex.WorkStatus
	releases                   map[string]chan struct{}
	released                   map[string]bool
	interrupted                map[string]bool
	started                    chan string
	runs, creations, continues map[string]int
	interrupts                 map[string]int
	interruptFailures          map[string]error
	inspects                   map[string]int
	observerOpens              int
	observerCloses             int
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		runStatuses:        map[string][]string{},
		beforeCreateErrors: map[string][]error{},
		continueStatuses:   map[string][]string{},
		inspectStatuses:    map[string][]codex.WorkStatus{},
		releases:           map[string]chan struct{}{},
		released:           map[string]bool{},
		interrupted:        map[string]bool{},
		started:            make(chan string, 16),
		runs:               map[string]int{},
		creations:          map[string]int{},
		continues:          map[string]int{},
		interrupts:         map[string]int{},
		interruptFailures:  map[string]error{},
		inspects:           map[string]int{},
	}
}

func (c *fakeClient) Run(ctx context.Context, command codex.Command) (codex.Result, error) {
	stepID := stepFromTitle(command.Title)
	threadID := "chat-" + stepID
	c.mu.Lock()
	c.runs[stepID]++
	beforeCreateErrors := c.beforeCreateErrors[stepID]
	var beforeCreateErr error
	if len(beforeCreateErrors) != 0 {
		beforeCreateErr = beforeCreateErrors[0]
		c.beforeCreateErrors[stepID] = beforeCreateErrors[1:]
	}
	statuses := c.runStatuses[stepID]
	status := "completed"
	if len(statuses) != 0 {
		status = statuses[0]
		c.runStatuses[stepID] = statuses[1:]
	}
	release := c.releases[stepID]
	c.mu.Unlock()
	if beforeCreateErr != nil {
		return codex.Result{}, beforeCreateErr
	}
	c.mu.Lock()
	c.creations[stepID]++
	c.mu.Unlock()
	if command.OnThread != nil {
		if err := command.OnThread(threadID); err != nil {
			return codex.Result{ThreadID: threadID, CreationAttempted: true}, err
		}
	}
	if command.OnTurn != nil {
		command.OnTurn("turn-"+stepID, func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.interrupt(stepID)
		})
	}
	if command.Notify != nil {
		if err := command.Notify(codex.Event{Method: "turn/started"}); err != nil {
			return codex.Result{ThreadID: threadID, CreationAttempted: true, TurnAttempted: true}, err
		}
	}
	c.started <- stepID
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return codex.Result{ThreadID: threadID, TurnID: "turn-" + stepID, CreationAttempted: true, TurnAttempted: true}, ctx.Err()
		}
	}
	c.mu.Lock()
	if c.interrupted[stepID] {
		status = "interrupted"
	}
	c.mu.Unlock()
	return codex.Result{ThreadID: threadID, TurnID: "turn-" + stepID, Status: status, CreationAttempted: true, TurnAttempted: true}, nil
}

func (c *fakeClient) Continue(ctx context.Context, threadID string, command codex.Command) (codex.Result, error) {
	stepID := strings.TrimPrefix(threadID, "chat-")
	c.mu.Lock()
	c.continues[stepID]++
	statuses := c.continueStatuses[stepID]
	status := "completed"
	if len(statuses) != 0 {
		status = statuses[0]
		c.continueStatuses[stepID] = statuses[1:]
	}
	switch status {
	case "completed":
		c.inspectStatuses[threadID] = []codex.WorkStatus{codex.WorkCompleted}
	case "failed":
		c.inspectStatuses[threadID] = []codex.WorkStatus{codex.WorkFailed}
	case "interrupted":
		c.inspectStatuses[threadID] = []codex.WorkStatus{codex.WorkInterrupted}
	}
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return codex.Result{ThreadID: threadID}, err
	}
	if command.OnTurn != nil {
		command.OnTurn("continued-"+stepID, func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.interrupt(stepID)
		})
	}
	if command.Notify != nil {
		if err := command.Notify(codex.Event{Method: "turn/started"}); err != nil {
			return codex.Result{ThreadID: threadID, TurnID: "continued-" + stepID, TurnAttempted: true}, err
		}
	}
	return codex.Result{ThreadID: threadID, TurnID: "continued-" + stepID, Status: status, TurnAttempted: true}, nil
}

func (c *fakeClient) interrupt(stepID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.interrupts[stepID]++
	failure := c.interruptFailures[stepID]
	if failure == nil {
		c.interrupted[stepID] = true
	}
	if release := c.releases[stepID]; release != nil && !c.released[stepID] {
		close(release)
		c.released[stepID] = true
	}
	return failure
}

// fakeObserver отделяет жизненный цикл одной polling-сессии от клиента активных
// turn. Счётчики доказывают переиспользование между чатами и циклами Execute.
type fakeObserver struct {
	client *fakeClient
	once   sync.Once
}

func (c *fakeClient) OpenObserver(_ context.Context, _ string) (Observer, error) {
	c.mu.Lock()
	c.observerOpens++
	c.mu.Unlock()
	return &fakeObserver{client: c}, nil
}

func (o *fakeObserver) Inspect(threadID string) (codex.Observation, error) {
	c := o.client
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inspects[threadID]++
	statuses := c.inspectStatuses[threadID]
	status := codex.WorkCompleted
	if len(statuses) != 0 {
		status = statuses[0]
		if len(statuses) > 1 {
			c.inspectStatuses[threadID] = statuses[1:]
		}
	}
	return observationFor(threadID, status), nil
}

func (o *fakeObserver) Close() error {
	o.once.Do(func() {
		o.client.mu.Lock()
		o.client.observerCloses++
		o.client.mu.Unlock()
	})
	return nil
}

func stepFromTitle(title string) string {
	parts := strings.SplitN(title, " / ", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.SplitN(parts[1], " [", 2)[0]
}

func observationFor(threadID string, status codex.WorkStatus) codex.Observation {
	observation := codex.Observation{ThreadID: threadID, ThreadStatus: "idle", LatestTurnID: "turn-1"}
	switch status {
	case codex.WorkUnknown:
		observation.LatestTurnID = ""
	case codex.WorkRunning:
		observation.ThreadStatus, observation.LatestTurnStatus = "active", "inProgress"
	case codex.WorkWaitingForApproval:
		observation.ThreadStatus, observation.LatestTurnStatus = "active", "inProgress"
		observation.ActiveFlags = []string{"waitingOnApproval"}
	case codex.WorkFailed:
		observation.LatestTurnStatus = "failed"
	case codex.WorkInterrupted:
		observation.LatestTurnStatus = "interrupted"
	case codex.WorkCompleted:
		observation.LatestTurnStatus = "completed"
	}
	return observation
}

func createExecutionRun(t *testing.T, workflowJSON string) (string, *runstore.LockedRun) {
	t.Helper()
	root := t.TempDir()
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(workflowJSON), Task: "Сделать MVP", CWD: t.TempDir(), InitiatorThreadID: "initiator",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return root, run
}

// controlledTicker позволяет продвинуть только нужные часы координатора. Тесты
// минутного статуса не зависят от скорости CI и не ждут реальную минуту.
type controlledTicker struct {
	events  chan time.Time
	mu      sync.Mutex
	stopped bool
}

// newControlledTicker создаёт буфер, чтобы тест не зависел от точного момента select.
func newControlledTicker() *controlledTicker {
	return &controlledTicker{events: make(chan time.Time, 8)}
}

// C отдаёт только события, явно добавленные тестом.
func (t *controlledTicker) C() <-chan time.Time { return t.events }

// Stop отмечает завершение цикла, не закрывая канал, в который ещё пишет тест.
func (t *controlledTicker) Stop() {
	t.mu.Lock()
	t.stopped = true
	t.mu.Unlock()
}

// isStopped читает признак под тем же mutex, что и Stop, чтобы race detector
// проверял именно поведение координатора, а не служебный код теста.
func (t *controlledTicker) isStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

// TestExecuteReportsEveryIntervalAndStopsAfterFinalState проверяет цикл issue #17.
// Изменение состояния публикуется сразу, неизменный Running — по управляемому
// минутному событию, а после финального Succeeded оба ticker останавливаются.
func TestExecuteReportsEveryIntervalAndStopsAfterFinalState(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"child","type":"agent","prompt":"Итог","dependsOn":["parent"]},{"id":"parent","type":"agent","prompt":"Факты","dependsOn":[]}]}`)
	client := newFakeClient()
	client.releases["parent"] = make(chan struct{})
	pollTicker, reportTicker := newControlledTicker(), newControlledTicker()
	unexpectedIntervals := make(chan time.Duration, 1)
	notifications := make(chan Status, 16)
	done := make(chan error, 1)
	go func() {
		done <- Execute(t.Context(), run, Options{
			Root: root, PollInterval: time.Second, ReportInterval: time.Minute, Client: client,
			NewTicker: func(interval time.Duration) Ticker {
				if interval == time.Second {
					return pollTicker
				}
				if interval == time.Minute {
					return reportTicker
				}
				unexpectedIntervals <- interval
				return newControlledTicker()
			},
			Notify: func(status Status) error {
				notifications <- status
				return nil
			},
		})
	}()

	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("родительский кубик не стартовал")
	}
	pollTicker.events <- time.Now()
	running := waitForStatus(t, notifications, func(status Status) bool {
		return len(status.Steps) == 2 && status.Steps[1].State == scheduler.Running
	})
	if running.WorkflowID != "flow" || len(running.Steps[0].DependsOn) != 1 || running.Steps[0].DependsOn[0] != "parent" {
		t.Fatalf("снимок потерял workflow или зависимости: %+v", running)
	}

	reportTicker.events <- time.Now()
	repeated := waitForStatus(t, notifications, func(status Status) bool {
		return len(status.Steps) == 2 && status.Steps[1].State == scheduler.Running
	})
	if len(repeated.Steps) != 2 {
		t.Fatalf("минутный список содержит не каждый кубик ровно один раз: %+v", repeated.Steps)
	}

	close(client.releases["parent"])
	final := waitForStatus(t, notifications, func(status Status) bool { return status.Complete })
	if len(final.Steps) != 2 || final.Steps[0].State != scheduler.Succeeded || final.Steps[1].State != scheduler.Succeeded {
		t.Fatalf("финальный снимок опубликован преждевременно: %+v", final)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("координатор не остановился после финального статуса")
	}
	if !pollTicker.isStopped() || !reportTicker.isStopped() {
		t.Fatal("после финального статуса минутный цикл не остановлен")
	}
	select {
	case interval := <-unexpectedIntervals:
		t.Fatalf("создан ticker с неожиданным интервалом %s", interval)
	default:
	}
	reportTicker.events <- time.Now()
	select {
	case status := <-notifications:
		t.Fatalf("после финала опубликован лишний статус: %+v", status)
	default:
	}
}

// waitForStatus пропускает промежуточные изменения и возвращает первый нужный
// снимок; секундный timeout остаётся только защитой от зависшего теста.
func waitForStatus(t *testing.T, statuses <-chan Status, match func(Status) bool) Status {
	t.Helper()
	for {
		select {
		case status := <-statuses:
			if match(status) {
				return status
			}
		case <-time.After(time.Second):
			t.Fatal("ожидаемый статус не опубликован")
		}
	}
}

// TestExecuteParallelChain доказывает две границы: независимые шаги уже запущены
// до завершения любого из них, а сборщик создаётся один раз только после обоих.
func TestExecuteParallelChain(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"summary","type":"agent","prompt":"Итог","dependsOn":["first","second"]},{"id":"first","type":"agent","prompt":"Первый","dependsOn":[]},{"id":"second","type":"agent","prompt":"Второй","dependsOn":[]}]}`)
	client := newFakeClient()
	client.releases["first"], client.releases["second"] = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Execute(t.Context(), run, Options{Root: root, PollInterval: time.Millisecond, Client: client})
	}()
	started := map[string]bool{}
	for len(started) < 2 {
		select {
		case step := <-client.started:
			started[step] = true
		case <-time.After(time.Second):
			t.Fatal("независимая волна не стартовала параллельно")
		}
	}
	if started["summary"] {
		t.Fatal("сборщик стартовал до зависимостей")
	}
	close(client.releases["first"])
	close(client.releases["second"])
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("workflow не завершился")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, step := range []string{"first", "second", "summary"} {
		if client.runs[step] != 1 {
			t.Errorf("шаг %q запущен %d раз", step, client.runs[step])
		}
	}
}

// TestExecuteManualContinuation проверяет главный resume-сценарий MVP: failed
// не создаёт второй чат, последующий completed открывает зависимый шаг.
func TestExecuteManualContinuation(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"child","type":"agent","prompt":"Итог","dependsOn":["parent"]},{"id":"parent","type":"agent","prompt":"Факты","dependsOn":[]}]}`)
	client := newFakeClient()
	client.runStatuses["parent"] = []string{"failed"}
	client.inspectStatuses["chat-parent"] = []codex.WorkStatus{codex.WorkFailed, codex.WorkCompleted}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := Execute(ctx, run, Options{Root: root, PollInterval: time.Millisecond, Client: client, ContinueInterrupted: true}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.runs["parent"] != 1 || client.continues["parent"] != 0 || client.runs["child"] != 1 || client.inspects["chat-parent"] < 2 {
		t.Fatalf("ручное продолжение создало дубликат или не открыло зависимость: runs=%v inspect=%v", client.runs, client.inspects)
	}
}

// TestExecuteReusesObserverAcrossPolling проверяет R4 на нескольких чатах:
// сколько бы thread/read ни понадобилось, Execute открывает и закрывает ровно одну
// read-only сессию. Failed оставляет flow незавершённым на несколько polling-циклов.
func TestExecuteReusesObserverAcrossPolling(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"one","type":"agent","prompt":"Один","dependsOn":[]},{"id":"two","type":"agent","prompt":"Два","dependsOn":[]},{"id":"three","type":"agent","prompt":"Три","dependsOn":[]}]}`)
	stepIDs := []string{"one", "two", "three"}
	if err := run.Reserve(stepIDs); err != nil {
		t.Fatal(err)
	}
	client := newFakeClient()
	for _, stepID := range stepIDs {
		threadID := "chat-" + stepID
		if err := run.Update(stepID, scheduler.Failed, threadID); err != nil {
			t.Fatal(err)
		}
		client.inspectStatuses[threadID] = []codex.WorkStatus{codex.WorkFailed}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := Execute(ctx, run, Options{Root: root, PollInterval: time.Millisecond, Client: client}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ожидалась остановка тестового polling: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.observerOpens != 1 || client.observerCloses != 1 {
		t.Fatalf("polling создал неверное число наблюдающих сессий: opens=%d closes=%d", client.observerOpens, client.observerCloses)
	}
	for _, stepID := range stepIDs {
		if count := client.inspects["chat-"+stepID]; count < 2 {
			t.Errorf("чат %q прочитан только %d раз: тест не прошёл несколько polling-циклов", stepID, count)
		}
	}
}

// TestExecuteResumeContinuesInterrupted проверяет новый resume-контракт: только
// interrupted получает один turn "continue" в прежнем чате, после чего успех
// открывает зависимость без создания второго чата родительского шага.
func TestExecuteResumeContinuesInterrupted(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"child","type":"agent","prompt":"Итог","dependsOn":["parent"]},{"id":"parent","type":"agent","prompt":"Факты","dependsOn":[]}]}`)
	if err := run.Reserve([]string{"parent"}); err == nil {
		err = run.Update("parent", scheduler.Cancelled, "chat-parent")
	} else {
		t.Fatal(err)
	}
	client := newFakeClient()
	client.inspectStatuses["chat-parent"] = []codex.WorkStatus{codex.WorkInterrupted}
	if err := Execute(t.Context(), run, Options{
		Root: root, PollInterval: time.Millisecond, Client: client, ContinueInterrupted: true,
	}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.runs["parent"] != 0 || client.continues["parent"] != 1 || client.runs["child"] != 1 {
		t.Fatalf("resume неверно продолжил interrupted-чат: runs=%v, continues=%v", client.runs, client.continues)
	}
}

// TestExecuteResumeDoesNotLoopInterrupted не позволяет одному resume бесконечно
// отправлять continue, если продолжённый агент снова завершился interrupted.
func TestExecuteResumeDoesNotLoopInterrupted(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"one","type":"agent","prompt":"Один","dependsOn":[]}]}`)
	if err := run.Reserve([]string{"one"}); err == nil {
		err = run.Update("one", scheduler.Cancelled, "chat-one")
	} else {
		t.Fatal(err)
	}
	client := newFakeClient()
	client.inspectStatuses["chat-one"] = []codex.WorkStatus{codex.WorkInterrupted}
	client.continueStatuses["one"] = []string{"interrupted"}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := Execute(ctx, run, Options{
		Root: root, PollInterval: time.Millisecond, Client: client, ContinueInterrupted: true,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ожидалась остановка теста после одного continue: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.continues["one"] != 1 {
		t.Fatalf("один resume отправил continue %d раз", client.continues["one"])
	}
}

// TestExecuteRejectsAmbiguousStarting воспроизводит перезапуск после потери
// ответа thread/start. Никакой другой готовый шаг не должен уйти в сеть.
func TestExecuteRejectsAmbiguousStarting(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"one","type":"agent","prompt":"Один","dependsOn":[]},{"id":"two","type":"agent","prompt":"Два","dependsOn":[]}]}`)
	if err := run.Reserve([]string{"one"}); err != nil {
		t.Fatal(err)
	}
	client := newFakeClient()
	err := Execute(t.Context(), run, Options{Root: root, PollInterval: time.Millisecond, Client: client})
	if !errors.Is(err, ErrAmbiguousStart) || !strings.Contains(err.Error(), "мог создать чат") || !strings.Contains(err.Error(), "не запускайте новый run") {
		t.Fatalf("неоднозначный запуск не распознан или не объяснён агенту: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.runs) != 0 {
		t.Fatalf("после неоднозначности начались новые запросы: %v", client.runs)
	}
}

// TestExecuteReleasesUnattemptedCreation воспроизводит безопасный сбой Codex до
// thread/start. Первый вызов обязан сохранить исходную диагностику и вернуть шаг
// в Pending; после повторного открытия явный resume создаёт ровно один чат.
func TestExecuteReleasesUnattemptedCreation(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"one","type":"agent","prompt":"Один","dependsOn":[]}]}`)
	failure := errors.New("initialize Codex: connection refused")
	client := newFakeClient()
	client.beforeCreateErrors["one"] = []error{failure}
	err := Execute(t.Context(), run, Options{Root: root, PollInterval: time.Millisecond, Client: client})
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "шаг возвращён в Pending") || !strings.Contains(err.Error(), "lawa resume <runId>") {
		t.Fatalf("агент не получил причину и безопасную рекомендацию: %v", err)
	}
	snapshot, loadErr := run.Load()
	if loadErr != nil || snapshot.Meta.Steps[0].State != scheduler.Pending || snapshot.Meta.Steps[0].CodexThreadID != "" {
		t.Fatalf("неотправленное создание заблокировало run: %+v, %v", snapshot.Meta.Steps, loadErr)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := Execute(t.Context(), reopened, Options{Root: root, PollInterval: time.Millisecond, Client: client}); err != nil {
		t.Fatalf("resume не повторил безопасное создание: %v", err)
	}
	client.mu.Lock()
	runs, creations := client.runs["one"], client.creations["one"]
	client.mu.Unlock()
	if runs != 2 || creations != 1 {
		t.Fatalf("ожидались один безопасный повтор и одно создание чата: runs=%d, creations=%d", runs, creations)
	}
	snapshot, err = reopened.Load()
	if err != nil || snapshot.Meta.Steps[0].State != scheduler.Succeeded || snapshot.Meta.Steps[0].CodexThreadID != "chat-one" {
		t.Fatalf("повтор не завершил исходный шаг: %+v, %v", snapshot.Meta.Steps, err)
	}
}

// TestExecuteCancellationInterruptsTurn проверяет локальный контракт сигнала:
// координатор адресно прерывает активный turn, сохраняет Cancelled и возвращает
// управление без ожидания искусственного завершения работы агентом.
func TestExecuteCancellationInterruptsTurn(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"one","type":"agent","prompt":"Один","dependsOn":[]}]}`)
	client := newFakeClient()
	release := make(chan struct{})
	client.releases["one"] = release
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Execute(ctx, run, Options{Root: root, PollInterval: time.Hour, Client: client})
	}()
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("turn не стартовал")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("неверная ошибка отмены: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("сигнал не прервал активный turn")
	}
	snapshot, err := run.Load()
	client.mu.Lock()
	interrupts := client.interrupts["one"]
	client.mu.Unlock()
	if err != nil || snapshot.Meta.Steps[0].State != scheduler.Cancelled || snapshot.Meta.Steps[0].CodexThreadID != "chat-one" || interrupts != 1 {
		t.Fatalf("не сохранена явная отмена turn: %+v, interrupts=%d, %v", snapshot.Meta.Steps, interrupts, err)
	}
}

// TestExecuteCancellationKeepsConcurrentCompletion закрывает гонку между
// естественным completed и turn/interrupt. Отказ interrupt в этот момент не
// должен стереть уже полученный успех состоянием Unknown.
func TestExecuteCancellationKeepsConcurrentCompletion(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"one","type":"agent","prompt":"Один","dependsOn":[]}]}`)
	client := newFakeClient()
	client.releases["one"] = make(chan struct{})
	client.interruptFailures["one"] = errors.New("turn уже завершён")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Execute(ctx, run, Options{Root: root, PollInterval: time.Hour, Client: client})
	}()
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("turn не стартовал")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("потерян сигнал: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("конкурентное завершение не остановило координатор")
	}
	snapshot, err := run.Load()
	if err != nil || snapshot.Meta.Steps[0].State != scheduler.Succeeded {
		t.Fatalf("успех потерян из-за гонки с interrupt: %+v, %v", snapshot.Meta.Steps, err)
	}
}

// TestExecuteErrorDrainsTurn распространяет гарантию сигнала на внутренние ошибки:
// отказ stdout прекращает новые волны, но уже созданный app-server не закрывается
// до терминального результата и постоянная связь остаётся корректной.
func TestExecuteErrorDrainsTurn(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"flow","steps":[{"id":"one","type":"agent","prompt":"Один","dependsOn":[]}]}`)
	client := newFakeClient()
	release := make(chan struct{})
	client.releases["one"] = release
	failure := errors.New("stdout unavailable")
	done := make(chan error, 1)
	go func() {
		done <- Execute(t.Context(), run, Options{
			Root: root, PollInterval: time.Hour, Client: client,
			Notify: func(Status) error { return failure },
		})
	}()
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("turn не стартовал")
	}
	select {
	case err := <-done:
		t.Fatalf("ошибка вывода оборвала активный turn: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("потеряна исходная ошибка вывода: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("координатор не завершился после активного turn")
	}
	snapshot, err := run.Load()
	if err != nil || snapshot.Meta.Steps[0].State != scheduler.Succeeded {
		t.Fatalf("терминальный статус не сохранён при ошибке вывода: %+v, %v", snapshot.Meta.Steps, err)
	}
}
