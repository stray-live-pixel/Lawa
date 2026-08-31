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

const (
	testExecutorThreadID = "01a05852-8c1a-72f2-b37c-fe281bc2b58d"
)

func createRun(t *testing.T, root, workflowID, parent string) runstore.Snapshot {
	return createRunWithTask(t, root, workflowID, parent, "Задача")
}

func createRunWithTask(t *testing.T, root, workflowID, parent, task string) runstore.Snapshot {
	t.Helper()
	workflow := `{"id":` + quoted(workflowID) + `,"steps":[{"id":"cube","type":"agent","prompt":"Сделай","dependsOn":[]}]}`
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(workflow), Task: task, CWD: t.TempDir(),
		ParentRunID: parent,
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
		err = run.Update("cube", state, testExecutorThreadID)
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
	child := createRunWithTask(t, root, "child-workflow", parent.Meta.RunID, `ISSUE_ID=THINKTWICE-592
TASK_TITLE=[СП] Проблемы с модалкой на уровнях
TRACKER_CONTEXT_BEGIN
{
  "id": "THINKTWICE-592",
  "url": "https://st.yandex-team.ru/THINKTWICE-592",
  "summary": "заголовок из контекста с меньшим приоритетом"
}
TRACKER_CONTEXT_END`)
	childRun, err := runstore.OpenLocked(root, child.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err = childRun.AppendEvent(runstore.RuntimeEvent{StepID: "cube", Kind: "process_started", PID: 4321, Message: "безопасное событие"}); err == nil {
		err = childRun.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
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
		"&lt;release&gt;&amp;", "child-workflow", "broken-run", "tone-running", "vscode://file/",
		"failed-workflow", "succeeded-workflow", "В работе", "Сломавшиеся", "Успешные",
		"Тикет · THINKTWICE-592", "[СП] Проблемы с модалкой на уровнях", "https://st.yandex-team.ru/THINKTWICE-592",
		"События", "Папка", "/events/" + child.Meta.RunID,
		"За последний час", "За последние 2 часа", "За последние 4 часа", "За последние 8 часов", "За последние 12 часов",
		"За последние 24 часа", "За последние 2 дня", "За последние 5 дней", "За последнюю неделю", "За последние 2 недели", "За последний месяц", "За всё время",
		"Flow, кубик, тикет, run/thread ID, текст задачи…",
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
	if strings.Contains(html, "codex://") || strings.Contains(html, "Тред JSONL") {
		t.Fatal("dashboard снова раскрыл нативные ссылки или сырой rollout Codex")
	}
	search := httptest.NewRecorder()
	dashboard.ServeHTTP(search, httptest.NewRequest(http.MethodGet, "/?period=all&q=thinktwice-592", nil))
	searchHTML := search.Body.String()
	if !strings.Contains(searchHTML, parent.Meta.RunID) || !strings.Contains(searchHTML, child.Meta.RunID) ||
		strings.Contains(searchHTML, failed.Meta.RunID) || strings.Contains(searchHTML, succeeded.Meta.RunID) ||
		!strings.Contains(searchHTML, `value="thinktwice-592"`) || !strings.Contains(searchHTML, `const searchActive= true ;`) {
		t.Fatal("поиск по тикету не сохранил дерево, значение поля или режим раскрытия результатов")
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
	recorder = httptest.NewRecorder()
	dashboard.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/events/"+child.Meta.RunID+"?step=cube", nil))
	if recorder.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !strings.Contains(recorder.Body.String(), "process_started") || !strings.Contains(recorder.Body.String(), "pid=4321") {
		t.Fatalf("события не отданы как безопасный текст: %s, %q", recorder.Header().Get("Content-Type"), recorder.Body.String())
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

// TestCorruptedEventsRemainVisible сохраняет сам run в дереве, но показывает
// оператору отдельную проблему журнала вместо молчаливой потери observability.
func TestCorruptedEventsRemainVisible(t *testing.T) {
	root := t.TempDir()
	run := createRun(t, root, "visible-with-broken-events", "")
	if err := os.WriteFile(filepath.Join(root, run.Meta.RunID, "events.jsonl"), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	Handler(root).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?period=all", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, "visible-with-broken-events") || !strings.Contains(body, "журнал событий") {
		t.Fatalf("повреждение журнала скрыто вместе с run: %q", body)
	}
}

// TestPreview гарантирует, что макет использует production-шаблон и остаётся
// достаточно сложным для визуальной оценки без run-хранилища.
func TestPreview(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/preview?period=all", nil)
	recorder := httptest.NewRecorder()
	Handler(filepath.Join(t.TempDir(), "missing")).ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, fragment := range []string{
		"fake data", "release-v0.3.0", "repair-failed-macos-build", "nightly-maintenance",
		"prepare-release-notes-with-a-deliberately-long-name", "tone-running", "tone-failed", "tone-succeeded", "skipped", "#preview",
		"В работе", "Сломавшиеся", "Успешные", "failed-nightly-cleanup", "previous-release", "За последние 24 часа", "За всё время",
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

// TestTicketFromTask фиксирует приоритет явных полей над JSON-контекстом и
// запрещает превращать опасную схему из постановки в активную кнопку.
func TestTicketFromTask(t *testing.T) {
	task := `ISSUE_ID=TEAM-42
TASK_TITLE=Явный заголовок
TRACKER_CONTEXT_BEGIN
{
  "id": "TEAM-42",
  "url": "https://tracker.example.test/TEAM-42",
  "summary": "Заголовок из JSON"
}
TRACKER_CONTEXT_END`
	got := ticketFromTask(task)
	if got.ID != "TEAM-42" || got.Title != "Явный заголовок" || string(got.URL) != "https://tracker.example.test/TEAM-42" {
		t.Fatalf("неверно извлечён тикет: %+v", got)
	}
	unsafe := ticketFromTask("ISSUE_ID=TEAM-1\nISSUE_URL=javascript:alert(1)")
	if unsafe.ID != "TEAM-1" || unsafe.URL != "" {
		t.Fatalf("опасная ссылка не отброшена: %+v", unsafe)
	}
	mismatched := ticketFromTask("ISSUE_ID=TEAM-1\nTRACKER_CONTEXT_BEGIN\n{\n\"id\": \"OTHER-2\",\n\"url\": \"https://tracker.example.test/OTHER-2\"\n}\nTRACKER_CONTEXT_END")
	if mismatched.ID != "TEAM-1" || mismatched.URL != "" {
		t.Fatalf("ссылка другого тикета ошибочно привязана к ID: %+v", mismatched)
	}
	if empty := ticketFromTask("Описание со ссылкой https://example.test/not-a-ticket"); empty != (ticketReference{}) {
		t.Fatalf("произвольная ссылка ошибочно распознана как тикет: %+v", empty)
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
