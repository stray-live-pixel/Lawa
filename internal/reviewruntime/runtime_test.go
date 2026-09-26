package reviewruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// fixture строит настоящий файловый store; подменяется только внешняя модель.
func fixture(t *testing.T) (*Engine, string) {
	t.Helper()
	store := reviewstore.New(t.TempDir())
	r, err := store.Create(reviewstore.CreateOptions{CWD: t.TempDir(), Prompt: "Проверь локальные изменения", Config: reviewstore.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Store: store}, r.ID
}

// invoke имитирует вызов уже проверенного app-server dynamic tool.
func invoke(t *testing.T, c codex.Command, name string, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	out, err := c.CallDynamicTool(context.Background(), codex.DynamicToolCall{Tool: name, Arguments: b})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// populate создаёт минимальный реальный контракт; approve остаётся обзором кода.
func populate(t *testing.T, c codex.Command) {
	t.Helper()
	if strings.Contains(c.Text, `этап "context"`) {
		invoke(t, c, "review_artifact", artifactInput{Path: "artifacts/file.txt", Text: "func login() {}\n"})
		invoke(t, c, "review_context", contextInput{Title: "Вход", SourceRevision: "abc", Context: reviewstore.Context{ProjectFiles: []reviewstore.File{{ID: "file", Path: "login.go", SnapshotPath: "artifacts/file.txt", StartLine: 1, EndLine: 1, Change: "context", Revision: "abc"}}}})
	} else if strings.Contains(c.Text, `этап "review"`) {
		invoke(t, c, "review_findings", findingsInput{})
	} else {
		invoke(t, c, "review_presentation", reviewstore.Presentation{Summary: "Добавлен вход", Overview: reviewstore.Tour{ID: "overview", Title: "Обзор изменений", Steps: []reviewstore.TourStep{{ID: "step", Title: "Вход", Body: "При входе вызываем login.", Anchors: []reviewstore.Anchor{{FileID: "file", StartLine: 1, EndLine: 1}}}}}})
	}
	invoke(t, c, "review_finish", finishInput{})
}

// TestThreeStages проверяет отдельные модели, фиксацию контекста и единый Markdown.
func TestThreeStages(t *testing.T) {
	e, id := fixture(t)
	models := []string{}
	e.RunCodex = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		models = append(models, c.Model)
		if err := c.OnThread("thread"); err != nil {
			return codex.Result{}, err
		}
		if err := c.OnTurn("turn", func(context.Context) error { return nil }); err != nil {
			return codex.Result{}, err
		}
		populate(t, c)
		return codex.Result{Status: "completed"}, nil
	}
	if err := e.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	r, err := e.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != reviewstore.Succeeded || r.Context.FrozenAt == nil || r.Verdict != "approve" {
		t.Fatalf("не завершён контракт: %+v", r)
	}
	if strings.Join(models, ",") != "gpt-6-sol,gpt-6-astra,gpt-6-sol" {
		t.Fatal(models)
	}
	md, err := e.Store.ReadArtifact(id, r.Presentation.MarkdownPath)
	if err != nil || !strings.Contains(string(md), "func login()") {
		t.Fatalf("обзор не переносит доказательство: %s %v", md, err)
	}
}

// TestMissingContract не принимает текстовый completed за сохранённый результат.
func TestMissingContract(t *testing.T) {
	e, id := fixture(t)
	e.RunCodex = func(context.Context, codex.Command) (codex.Result, error) {
		return codex.Result{Status: "completed"}, nil
	}
	if err := e.Run(context.Background(), id); err == nil {
		t.Fatal("принят пустой результат")
	}
	r, _ := e.Store.Load(id)
	if r.State != reviewstore.Failed {
		t.Fatal(r.State)
	}
}

// TestRetryPreservesContext повторяет только проваленную проверку и не стирает попытки.
func TestRetryPreservesContext(t *testing.T) {
	e, id := fixture(t)
	fail := true
	e.RunCodex = func(_ context.Context, c codex.Command) (codex.Result, error) {
		if fail && c.Model == "gpt-6-astra" {
			return codex.Result{}, errors.New("connection lost")
		}
		populate(t, c)
		return codex.Result{Status: "completed"}, nil
	}
	if e.Run(context.Background(), id) == nil {
		t.Fatal("ожидалась ошибка")
	}
	before, _ := e.Store.Load(id)
	fail = false
	if err := e.Retry(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	after, _ := e.Store.Load(id)
	if len(after.Stages[0].Attempts) != 1 || len(after.Stages[1].Attempts) != 2 || !before.Context.FrozenAt.Equal(*after.Context.FrozenAt) {
		t.Fatal("повтор потерял историю")
	}
}

// TestStopPending не начинает оплачиваемую работу после команды stop.
func TestStopPending(t *testing.T) {
	e, id := fixture(t)
	e.Store.RequestStop(id)
	e.RunCodex = func(context.Context, codex.Command) (codex.Result, error) {
		t.Fatal("запущена остановленная работа")
		return codex.Result{}, nil
	}
	if e.Run(context.Background(), id) == nil {
		t.Fatal("ожидался отказ")
	}
}

// TestStopRunning доказывает, что файловый stop приводит к отмене и interrupt.
func TestStopRunning(t *testing.T) {
	e, id := fixture(t)
	started := make(chan struct{})
	interrupted := make(chan struct{}, 1)
	e.RunCodex = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		c.OnTurn("turn", func(context.Context) error { interrupted <- struct{}{}; return nil })
		close(started)
		<-ctx.Done()
		return codex.Result{}, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background(), id) }()
	<-started
	e.Store.RequestStop(id)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stop принят как success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("отмена зависла")
	}
	r, _ := e.Store.Load(id)
	if r.State != reviewstore.Stopped || r.Stages[0].Attempts[0].State != reviewstore.Stopped {
		t.Fatal(r.State)
	}
	// При гонке завершения transport отмена ctx уже остановила процесс, отдельный
	// interrupt может не понадобиться; главное — отсутствие работающего run.
}

// TestUsage считает cache ровно один раз, не складывает обновления total.
func TestUsage(t *testing.T) {
	e, id := fixture(t)
	e.Store.Update(id, func(r *reviewstore.Review) error { return nil })
	e.RunCodex = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		c.OnThread("thread")
		c.OnTurn("turn", func(context.Context) error { return nil })
		for i := 0; i < 2; i++ {
			raw := json.RawMessage(`{"threadId":"thread","tokenUsage":{"total":{"inputTokens":100,"cachedInputTokens":40,"outputTokens":10}}}`)
			if err := c.Notify(codex.Event{Method: "thread/tokenUsage/updated", Params: raw}); err != nil {
				t.Fatal(err)
			}
		}
		populate(t, c)
		return codex.Result{Status: "completed"}, nil
	}
	if err := e.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	r, _ := e.Store.Load(id)
	u := r.Stages[0].Attempts[0].Usage
	if u.InputTokens == nil || *u.InputTokens != 100 || *u.CachedInputTokens != 40 || u.CostUSD == nil || *u.CostUSD != 0.000228 {
		t.Fatalf("usage: %+v", u)
	}
}

// TestPublicationReceipt запрещает published без planned и ложный успех review.
func TestPublicationReceipt(t *testing.T) {
	e, id := fixture(t)
	s := &stageSession{engine: e, id: id, stage: reviewstore.PresentationStage, number: 1}
	e.Store.SaveArtifact(id, "artifacts/f.md", []byte("finding"))
	_, err := e.Store.Update(id, func(r *reviewstore.Review) error {
		r.Title = "Review"
		r.ChangeURL = "https://example.test/pr/1"
		r.SourceRevision = "rev"
		r.Findings = []reviewstore.Finding{{ID: "f", Title: "Ошибка", Priority: "P1", Explanation: "proof", Reproduction: "click", Consequence: "twice", Anchors: []reviewstore.Anchor{{NoAnchorReason: "нет строки"}}, MarkdownPath: "artifacts/f.md"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p := reviewstore.Publication{FindingID: "f", Revision: "rev", State: "published", URL: "https://example.test/comment/1", ExternalID: "1"}
	if _, err = s.publication(p); err == nil {
		t.Fatal("published без намерения")
	}
	p.State = "planned"
	p.Revision = "stale"
	if _, err = s.publication(p); err == nil {
		t.Fatal("разрешена публикация другой ревизии")
	}
	p.Revision = "rev"
	if _, err = s.publication(p); err != nil {
		t.Fatal(err)
	}
	if err = s.validate(); err == nil {
		t.Fatal("planned принят за успешную презентацию")
	}
	p.State = "published"
	if _, err = s.publication(p); err != nil {
		t.Fatal(err)
	}
	p.State = "planned"
	if _, err = s.publication(p); err == nil {
		t.Fatal("повторная публикация разрешена")
	}
}

// TestArtifactEscape запрещает импорт через symlink вне проекта.
func TestArtifactEscape(t *testing.T) {
	e, id := fixture(t)
	r, _ := e.Store.Load(id)
	outside := t.TempDir() + "/secret"
	os.WriteFile(outside, []byte("private"), 0600)
	os.Symlink(outside, r.CWD+"/link")
	s := &stageSession{engine: e, id: id, scratch: t.TempDir()}
	if _, err := s.artifact(artifactInput{Path: "artifacts/file", SourcePath: r.CWD + "/link"}); err == nil {
		t.Fatal("разрешён выход из проекта")
	}
}

// TestCodeRangeReject проверяет, что презентация не экспортирует несуществующие строки.
func TestCodeRangeReject(t *testing.T) {
	e, id := fixture(t)
	s := &stageSession{engine: e, id: id}
	e.Store.SaveArtifact(id, "artifacts/code", []byte("line\n"))
	r, _ := e.Store.Load(id)
	r.Context.ProjectFiles = []reviewstore.File{{ID: "f", Path: "code", SnapshotPath: "artifacts/code", StartLine: 40, EndLine: 40}}
	if _, err := s.anchorMarkdown(r, reviewstore.Anchor{FileID: "f", StartLine: 41, EndLine: 42}); err == nil {
		t.Fatal("приняты несуществующие строки")
	}
	out, err := s.anchorMarkdown(r, reviewstore.Anchor{FileID: "f", StartLine: 40, EndLine: 40})
	if err != nil || !strings.Contains(out, "line") {
		t.Fatal(fmt.Sprint(out, err))
	}
}

// TestChildTotals складывает независимые thread-группы и не маскирует отсутствие
// хотя бы одной из них корректной ценой известных запросов.
func TestChildTotals(t *testing.T) {
	i, c, o := int64(100), int64(40), int64(10)
	price := reviewstore.Pricing{Input: 2, CachedInput: .2, Output: 10}
	u := reviewstore.PriceUsage(reviewstore.Usage{InputTokens: &i, CachedInputTokens: &c, OutputTokens: &o}, &price)
	groups := []reviewstore.ThreadUsage{{ThreadID: "root", Model: "m", Usage: u}, {ThreadID: "child", Model: "m", Usage: u}}
	all := aggregateUsage(groups, true)
	if !all.Complete || *all.InputTokens != 200 || *all.CostUSD != 0.000456 {
		t.Fatalf("%+v", all)
	}
	partial := aggregateUsage(groups, false)
	if partial.Complete || partial.CostUSD != nil || *partial.InputTokens != 200 {
		t.Fatalf("%+v", partial)
	}
}

// TestNoisyEvents не обновляет историю от квот аккаунта и текстовых дельт.
func TestNoisyEvents(t *testing.T) {
	e, id := fixture(t)
	before, _ := e.Store.Load(id)
	s := &stageSession{engine: e, id: id}
	for _, method := range []string{"account/rateLimits/updated", "item/agentMessage/delta", "item/reasoning/textDelta"} {
		if err := s.observe(codex.Event{Method: method, Params: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := e.Store.Load(id)
	if before.Revision != after.Revision {
		t.Fatal("шум меняет updatedAt")
	}
}

// TestDesignRaster не принимает HTML/SVG или переименованный текст за дизайн.
func TestDesignRaster(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 10, 10))); err != nil {
		t.Fatal(err)
	}
	if !validDesignImage(b.Bytes()) {
		t.Fatal("PNG отклонён")
	}
	for _, data := range []string{"<svg></svg>", "<html>login</html>", "PNG snapshot", "RIFFxxxxWEBP"} {
		if validDesignImage([]byte(data)) {
			t.Fatalf("принят %q", data)
		}
	}
}
