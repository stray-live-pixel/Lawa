package dashboard

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/statusreport"
)

func createRun(t *testing.T, root, workflowID, parent string) runstore.Snapshot {
	t.Helper()
	workflow := `{"id":` + quoted(workflowID) + `,"steps":[{"id":"cube","type":"agent","prompt":"Сделай","dependsOn":[]}]}`
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(workflow), Task: "Задача", CWD: t.TempDir(),
		InitiatorThreadID: "initiator-" + workflowID, ParentRunID: parent,
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func quoted(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// addFutureMetadataFields имитирует meta.json, записанный следующей Lawa. Поля
// добавлены и на верхнем уровне, и внутрь шага — именно второй случай ломал
// старый dashboard после появления revision.
func addFutureMetadataFields(t *testing.T, root, runID string) {
	t.Helper()
	path := filepath.Join(root, runID, "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := bytes.Replace(data, []byte(`"steps":[{`), []byte(`"futureRunField":true,"steps":[{"futureStepField":"ignored",`), 1)
	if bytes.Equal(data, updated) {
		t.Fatal("тест не добавил будущие поля в meta.json")
	}
	if err = os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
}

func setState(t *testing.T, root string, snapshot runstore.Snapshot, state scheduler.State) {
	t.Helper()
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Reserve([]string{"cube"}); err == nil {
		err = run.Update("cube", state, "codex-cube")
	}
	if closeErr := run.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestDashboard показывает дерево из сохранённых run, но изолирует повреждённый
// каталог. Специальные символы в именах экранируются шаблоном, а deeplink строятся
// только из проверенных snapshot.
func TestDashboard(t *testing.T) {
	root := t.TempDir()
	parent := createRun(t, root, `<release>&`, "")
	setState(t, root, parent, scheduler.Running)
	child := createRun(t, root, "child-workflow", parent.Meta.RunID)
	failed := createRun(t, root, "failed-workflow", "")
	setState(t, root, failed, scheduler.Failed)
	succeeded := createRun(t, root, "succeeded-workflow", "")
	setState(t, root, succeeded, scheduler.Succeeded)
	memory := filepath.Join(root, child.Meta.RunID, "memory", child.Meta.Steps[0].ThreadID+".md")
	if err := os.WriteFile(memory, []byte(`<script>alert("memory")</script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\npreview")
	if err := os.WriteFile(filepath.Join(root, parent.Meta.RunID, statusreport.ImageFilename), png, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "broken-run"), 0o700); err != nil {
		t.Fatal(err)
	}
	addFutureMetadataFields(t, root, parent.Meta.RunID)
	addFutureMetadataFields(t, root, child.Meta.RunID)

	dashboard := Handler(root)
	recorder := httptest.NewRecorder()
	dashboard.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	html := recorder.Body.String()
	for _, fragment := range []string{
		"&lt;release&gt;&amp;", "child-workflow", "broken-run", "tone-running", "codex://threads/", "vscode://file/",
		"failed-workflow", "succeeded-workflow", "В работе", "Сломавшиеся", "Успешные",
		"/uml/" + parent.Meta.RunID, "/memory/" + child.Meta.RunID + "/" + child.Meta.Steps[0].ThreadID,
	} {
		if !strings.Contains(html, fragment) {
			t.Errorf("на странице нет %q", fragment)
		}
	}
	if strings.Index(html, parent.Meta.RunID) > strings.Index(html, child.Meta.RunID) {
		t.Fatal("дочерний workflow показан вне родителя")
	}
	active, broken, successful := strings.Index(html, `data-group="active" open`), strings.Index(html, `data-group="failed"`), strings.Index(html, `data-group="succeeded"`)
	if active < 0 || broken < active || successful < broken {
		t.Fatalf("секции отсутствуют или нарушен порядок: active=%d failed=%d succeeded=%d", active, broken, successful)
	}
	if strings.Contains(html, `data-group="failed" open`) || strings.Contains(html, `data-group="succeeded" open`) {
		t.Fatal("сломавшиеся или успешные workflow раскрыты по умолчанию")
	}

	recorder = httptest.NewRecorder()
	dashboard.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/memory/"+child.Meta.RunID+"/"+child.Meta.Steps[0].ThreadID, nil))
	if recorder.Header().Get("Content-Type") != "text/plain; charset=utf-8" || recorder.Body.String() != `<script>alert("memory")</script>` {
		t.Fatalf("память исполнена как HTML или искажена: %s, %q", recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	dashboard.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/uml/"+parent.Meta.RunID, nil))
	if recorder.Header().Get("Content-Type") != "image/png" || recorder.Body.String() != string(png) {
		t.Fatalf("неверный UML: %s, %q", recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
}

// TestProtectedRoutes не позволяет URL выбрать чужой файл или memory другого
// run. ServeMux и повторная проверка snapshot дают 404 до файлового чтения.
func TestProtectedRoutes(t *testing.T) {
	root := t.TempDir()
	run := createRun(t, root, "safe", "")
	dashboard := Handler(root)
	for _, path := range []string{
		"/memory/" + run.Meta.RunID + "/" + strings.Repeat("0", 32),
		"/memory/not-a-run/not-a-thread", "/uml/not-a-run", "/memory/../../etc/passwd",
	} {
		recorder := httptest.NewRecorder()
		dashboard.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("опасный путь %q получил %d", path, recorder.Code)
		}
	}
}

// TestPreview гарантирует, что макет использует production-шаблон и остаётся
// достаточно сложным для визуальной оценки без run-хранилища.
func TestPreview(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/preview", nil)
	recorder := httptest.NewRecorder()
	Handler(filepath.Join(t.TempDir(), "missing")).ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, fragment := range []string{
		"fake data", "release-v0.3.0", "repair-failed-macos-build", "nightly-maintenance",
		"prepare-release-notes-with-a-deliberately-long-name", "tone-running", "tone-failed", "tone-succeeded", "skipped", "#preview",
		"В работе", "Сломавшиеся", "Успешные", "failed-nightly-cleanup", "previous-release",
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("preview не показывает %q", fragment)
		}
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("статичный preview неожиданно начал polling")
	}
	live := httptest.NewRecorder()
	Handler(filepath.Join(t.TempDir(), "missing")).ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(live.Body.String(), "Запусков пока нет") || !strings.Contains(live.Body.String(), `http-equiv="refresh"`) {
		t.Fatal("пустой live dashboard не показывает подсказку или polling")
	}
}

// TestGroupRuns фиксирует продуктовую классификацию и не позволяет случайно
// перенести ожидающий workflow в завершённые либо изменить порядок секций.
func TestGroupRuns(t *testing.T) {
	roots := []*runNode{
		{ID: "pending", State: "pending"}, {ID: "running", State: "running"},
		{ID: "failed", State: "failed"}, {ID: "succeeded", State: "succeeded"},
	}
	groups := groupRuns(roots)
	if len(groups) != 3 || groups[0].ID != "active" || !groups[0].Open || len(groups[0].Roots) != 2 ||
		groups[1].ID != "failed" || groups[1].Open || len(groups[1].Roots) != 1 ||
		groups[2].ID != "succeeded" || groups[2].Open || len(groups[2].Roots) != 1 {
		t.Fatalf("неверная группировка workflow: %+v", groups)
	}
}

// TestParentCycle защищает рекурсивный шаблон от вручную повреждённой пары run.
func TestParentCycle(t *testing.T) {
	a := &runNode{ID: "a", ParentID: "b"}
	b := &runNode{ID: "b", ParentID: "a"}
	if !parentCycle(a, map[string]*runNode{"a": a, "b": b}) {
		t.Fatal("цикл parentRunId не обнаружен")
	}
}

func TestLoopbackAndShutdown(t *testing.T) {
	for address, want := range map[string]bool{
		DefaultAddress: true, "localhost:60800": true, "[::1]:60800": true,
		"0.0.0.0:60800": false, ":60800": false, "broken": false,
	} {
		if got := IsLoopbackAddress(address); got != want {
			t.Errorf("IsLoopbackAddress(%q)=%v, ожидалось %v", address, got, want)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	listener := &blockingListener{closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, listener, Handler(t.TempDir())) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("сервер не завершился по контексту: %v", err)
	}
}

type blockingListener struct {
	once   sync.Once
	closed chan struct{}
}

func (l *blockingListener) Accept() (net.Conn, error) { <-l.closed; return nil, net.ErrClosed }
func (l *blockingListener) Close() error              { l.once.Do(func() { close(l.closed) }); return nil }
func (l *blockingListener) Addr() net.Addr            { return testAddress("dashboard-test") }

type testAddress string

func (a testAddress) Network() string { return "test" }
func (a testAddress) String() string  { return string(a) }
