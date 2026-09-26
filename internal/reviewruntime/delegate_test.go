package reviewruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// runningReview готовит попытку для регрессий реальных событий без модели.
func runningReview(t *testing.T) (*Engine, string, *stageSession) {
	t.Helper()
	e, id := fixture(t)
	_, err := e.Store.Update(id, func(r *reviewstore.Review) error {
		r.CurrentStage = reviewstore.ReviewStage
		r.State = reviewstore.Running
		r.Stages[1].State = reviewstore.Running
		r.Stages[1].Attempts = []reviewstore.Attempt{{Number: 1, State: reviewstore.Running, StartedAt: time.Now().UTC(), ThreadID: "parent"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, id, &stageSession{engine: e, id: id, stage: reviewstore.ReviewStage, number: 1, scratch: t.TempDir()}
}

// TestNativeSubAgentActivity повторяет фактическую форму Codex: spawnAgent
// отсутствует, приходят started/completed и обратное interacted к родителю.
func TestNativeSubAgentActivity(t *testing.T) {
	e, id, s := runningReview(t)
	for _, kind := range []string{"started", "started", "completed"} {
		data := json.RawMessage(`{"threadId":"parent","item":{"type":"subAgentActivity","id":"call","kind":"` + kind + `","agentThreadId":"child","agentPath":"/root/formula"}}`)
		if err := s.observe(codex.Event{Method: "item/completed", Params: data}); err != nil {
			t.Fatal(err)
		}
	}
	s.observe(codex.Event{Method: "item/completed", Params: json.RawMessage(`{"threadId":"child","item":{"type":"subAgentActivity","id":"back","kind":"interacted","agentThreadId":"parent","agentPath":"/root"}}`)})
	s.observe(codex.Event{Method: "thread/tokenUsage/updated", Params: json.RawMessage(`{"threadId":"parent","tokenUsage":{"total":{"inputTokens":100,"cachedInputTokens":20,"outputTokens":5}}}`)})
	r, _ := e.Store.Load(id)
	if len(r.Activities) != 1 || r.Activities[0].State != "completed" || r.Activities[0].ThreadID != "child" || !s.childSeen {
		t.Fatalf("не распознан реальный ребёнок: %+v", r.Activities)
	}
	if r.Stages[1].Attempts[0].Usage.Complete || r.Stages[1].Attempts[0].Usage.CostUSD != nil {
		t.Fatal("отсутствующее usage ребёнка скрыто")
	}
}

// TestManagedDelegate сохраняет точный вход и живые usage без чтения приватных
// rollout. Повтор CallID возвращает сохранённый ответ и не запускает модель дважды.
func TestManagedDelegate(t *testing.T) {
	e, id, s := runningReview(t)
	calls := 0
	s.usage([]byte(`{"threadId":"parent","tokenUsage":{"total":{"inputTokens":100,"cachedInputTokens":20,"outputTokens":5}}}`))
	e.RunCodex = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		calls++
		if !strings.Contains(c.Text, "Проверь формулу") || len(c.DynamicTools) != 1 || c.DynamicTools[0].Name != "review_read_skill" {
			t.Fatal("искажено поручение или полномочия")
		}
		if err := c.OnThread("managed-child"); err != nil {
			return codex.Result{}, err
		}
		c.Notify(codex.Event{Method: "thread/tokenUsage/updated", Params: json.RawMessage(`{"threadId":"managed-child","tokenUsage":{"total":{"inputTokens":200,"cachedInputTokens":40,"outputTokens":10}}}`)})
		c.Notify(codex.Event{Method: "item/completed", Params: json.RawMessage(`{"threadId":"managed-child","item":{"type":"agentMessage","text":"Формула нарушает требование"}}`)})
		return codex.Result{Status: "completed"}, nil
	}
	for i := 0; i < 2; i++ {
		out, err := s.delegate(context.Background(), "unique", delegateInput{Title: "Формула", Prompt: "Проверь формулу"})
		if err != nil || !strings.Contains(out, "Формула нарушает") {
			t.Fatalf("%s %v", out, err)
		}
	}
	r, _ := e.Store.Load(id)
	a := r.Stages[1].Attempts[0]
	if calls != 1 || len(a.ThreadUsage) != 2 || !a.Usage.Complete || *a.Usage.InputTokens != 300 {
		t.Fatalf("calls=%d attempt=%+v", calls, a)
	}
	if len(r.Activities) != 1 || r.Activities[0].State != "completed" || r.Activities[0].DocumentPath == "" {
		t.Fatal(r.Activities)
	}
}

// TestDelegateCancellation не оставляет ребёнка исполняться после остановки
// родителя и сохраняет отличие прерывания от успешного ответа.
func TestDelegateCancellation(t *testing.T) {
	e, id, s := runningReview(t)
	started := make(chan struct{})
	e.RunCodex = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		if err := c.OnThread("child"); err != nil {
			return codex.Result{}, err
		}
		if c.Permissions == nil || len(c.Permissions.ReadPaths) < 2 || c.Permissions.WritePaths[0] != s.scratch {
			t.Error("права ребёнка отличаются")
		}
		close(started)
		<-ctx.Done()
		return codex.Result{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.delegate(ctx, "cancelled", delegateInput{Title: "Проверка", Prompt: "Проверь"})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("отмена принята за успех")
		}
	case <-time.After(time.Second):
		t.Fatal("субагент не отменился")
	}
	r, _ := e.Store.Load(id)
	if r.Activities[0].State != "interrupted" {
		t.Fatal(r.Activities[0].State)
	}
}

// TestNativeFixtureReplay — необязательный read-only аудит сохранённого smoke.
// Изменяется только копия в t.TempDir; API thread/read/account/usage не запускает turn.
func TestNativeFixtureReplay(t *testing.T) {
	source := os.Getenv("LAWA_REVIEW_NATIVE_FIXTURE")
	if source == "" {
		t.Skip("нет локальной smoke-фикстуры")
	}
	id := filepath.Base(source)
	root := t.TempDir()
	if err := os.CopyFS(filepath.Join(root, id), os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Store: reviewstore.New(root)}
	r, err := e.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	events, err := e.Store.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	s := &stageSession{engine: e, id: id, stage: reviewstore.ReviewStage, number: len(r.Stages[1].Attempts), eventPrefix: "replay"}
	for _, event := range events {
		if event.Stage != reviewstore.ReviewStage {
			continue
		}
		raw, _ := json.Marshal(event.Data)
		var data struct{ Item struct{ Type string } }
		json.Unmarshal(raw, &data)
		if data.Item.Type == "subAgentActivity" {
			if err := s.observe(codex.Event{Method: event.Kind, Params: raw}); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.collectChildUsage()
	r, err = e.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range r.Activities {
		if a.Kind == "subagent" {
			found = true
			t.Logf("child=%s model=%s effort=%s state=%s promptAvailable=%t", a.ThreadID, a.Model, a.Effort, a.State, a.DocumentPath != "")
		}
	}
	if !found {
		t.Fatal("не найден реальный субагент")
	}
	t.Logf("aggregate complete=%t groups=%d", r.Stages[1].Attempts[0].Usage.Complete, len(r.Stages[1].Attempts[0].ThreadUsage))
}
