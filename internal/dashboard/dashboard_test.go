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
	"time"

	"github.com/stray-live-pixel/Lawa/assets"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/series"
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
	setState(t, root, child, scheduler.Running)
	childRun, err := runstore.OpenLocked(root, child.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err = childRun.AppendEvent(runstore.RuntimeEvent{StepID: "cube", Kind: "process_started", PID: 4321, Message: "безопасное событие"}); err == nil {
		err = childRun.AppendEvent(runstore.RuntimeEvent{StepID: "cube", Kind: "item_started", ItemID: "mcp-1", ItemType: "mcpToolCall", Content: "github · get_issue"})
	}
	if err == nil {
		err = childRun.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	failed := createRun(t, root, "failed-workflow", "")
	setState(t, root, failed, scheduler.Failed)
	succeeded := createRun(t, root, "succeeded-workflow", "")
	setState(t, root, succeeded, scheduler.Succeeded)
	planned, err := series.Create(root, series.Config{Mode: series.Cron, Cron: "0 10 * * *", TimeZone: "Europe/Moscow", MaxRuns: 10}, "scheduled-workflow")
	if err != nil {
		t.Fatal(err)
	}
	defer planned.Close()
	plannedAt := time.Now().Add(time.Hour).Truncate(time.Second)
	if err = planned.SetNext(plannedAt); err != nil {
		t.Fatal(err)
	}
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
		"failed-workflow", "succeeded-workflow", "0/1 шагов", "1/1 шагов", "Работа агента",
		"Тикет · THINKTWICE-592", "[СП] Проблемы с модалкой на уровнях", "https://st.yandex-team.ru/THINKTWICE-592",
		"События", "Папка", "/events/" + child.Meta.RunID, "/api/trace/" + child.Meta.RunID, "действие: mcpToolCall",
		"За последний час", "За последние 2 часа", "За последние 4 часа", "За последние 8 часов", "За последние 12 часов",
		"За последние 24 часа", "За последние 2 дня", "За последние 5 дней", "За последнюю неделю", "За последние 2 недели", "За последний месяц", "За всё время",
		"Flow, кубик, тикет, run/thread ID, текст задачи…",
		"selectionInside", "editableFocusInside", "schedulePanelOpen", "freshMarkup===dashboardMarkup", "traceRenderPending",
		"/assets/lawa-logo.png", "Расписание запусков", "data-schedule-open", "next-run-time", "scheduled-workflow", "Запуск: " + plannedAt.Local().Format("02.01.2006 15:04:05"), "cron 0 10 * * * · Europe/Moscow",
		"/uml/" + parent.Meta.RunID, "/memory/" + child.Meta.RunID + "/" + child.Meta.Steps[0].ThreadID,
	} {
		if !strings.Contains(html, fragment) {
			t.Errorf("на странице нет %q", fragment)
		}
	}
	if strings.Index(html, parent.Meta.RunID) > strings.Index(html, child.Meta.RunID) {
		t.Fatal("дочерний workflow показан вне родителя")
	}
	if strings.Contains(html, `data-group=`) || strings.Contains(html, `data-node="`+succeeded.Meta.RunID+`" open`) {
		t.Fatal("workflow снова разбиты по статусам или завершённый run раскрыт по умолчанию")
	}
	if strings.Contains(html, "codex://") || strings.Contains(html, "Тред JSONL") {
		t.Fatal("dashboard снова раскрыл нативные ссылки или сырой rollout Codex")
	}
	logo := httptest.NewRecorder()
	dashboard.ServeHTTP(logo, httptest.NewRequest(http.MethodGet, "/assets/lawa-logo.png", nil))
	if logo.Code != http.StatusOK || logo.Header().Get("Content-Type") != "image/png" || !bytes.Equal(logo.Body.Bytes(), assets.LawaLogoPNG) {
		t.Fatalf("dashboard не отдал встроенный логотип: status=%d content-type=%q size=%d", logo.Code, logo.Header().Get("Content-Type"), logo.Body.Len())
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
	if recorder.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !strings.Contains(recorder.Body.String(), "process_started") || !strings.Contains(recorder.Body.String(), "pid=4321") || strings.Contains(recorder.Body.String(), "github") {
		t.Fatalf("события не отданы как безопасный текст: %s, %q", recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	dashboard.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/trace/"+child.Meta.RunID+"?step=cube&after=0", nil))
	var trace traceResponse
	if err = json.Unmarshal(recorder.Body.Bytes(), &trace); err != nil || recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		len(trace.Events) != 1 || trace.Events[0].Content != "github · get_issue" || trace.Next == 0 {
		t.Fatalf("live-поток не отдан панели: status=%d trace=%+v err=%v", recorder.Code, trace, err)
	}
}

// TestScheduledRuns показывает только будущие решения активных серий. Просроченная
// точка остаётся первой как диагностика задержанного координатора, stop-маркер
// исключает уже запрещённый запуск, а повреждение одной серии не скрывает остальные.
func TestScheduledRuns(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var owners []*series.LockedSeries
	create := func(workflowID string, config series.Config, next time.Time) *series.LockedSeries {
		t.Helper()
		owner, err := series.Create(root, config, workflowID)
		if err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner)
		if err = owner.SetNext(next); err != nil {
			t.Fatal(err)
		}
		return owner
	}
	overdue := create("overdue", series.Config{Mode: series.Immediate}, now.Add(-time.Minute))
	after := create("after", series.Config{Mode: series.After, Delay: "30m", MaxRuns: 5}, now.Add(time.Hour))
	cron := create("cron", series.Config{Mode: series.Cron, Cron: "0 3 * * *", TimeZone: "Europe/Moscow"}, now.Add(2*time.Hour))
	stopped := create("stopped", series.Config{Mode: series.After, Delay: "1h"}, now.Add(30*time.Minute))
	if err := series.RequestStop(root, stopped.Snapshot().SeriesID); err != nil {
		t.Fatal(err)
	}
	for _, owner := range owners {
		defer owner.Close()
	}
	brokenID := strings.Repeat("a", 32)
	if err := os.MkdirAll(filepath.Join(root, "series", brokenID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "series", brokenID, "series.json"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	items, problems := loadScheduledRuns(root, now)
	if len(items) != 3 || items[0].SeriesID != overdue.Snapshot().SeriesID || !items[0].Overdue || items[0].Remaining != "ожидает запуска" ||
		items[1].SeriesID != after.Snapshot().SeriesID || items[1].Remaining != "1 ч" || items[1].Next != now.Add(time.Hour).Local().Format("02.01.2006 15:04:05") || items[1].Schedule != "через 30m после завершения" || items[1].Progress != "запущено: 0 из 5" ||
		items[2].SeriesID != cron.Snapshot().SeriesID || items[2].Overdue || len(problems) != 1 || problems[0].Name != "Серия "+brokenID {
		t.Fatalf("неверные запланированные серии: items=%+v problems=%+v", items, problems)
	}
}

// TestRemainingLabel фиксирует короткие подписи интерфейса без лишнего «через»:
// они округляются вверх, остаются компактными на больших интервалах и отдельно
// показывают просрочку.
func TestRemainingLabel(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		next time.Time
		want string
	}{
		{name: "overdue", next: now.Add(-time.Second), want: "ожидает запуска"},
		{name: "seconds", next: now.Add(20 * time.Second), want: "меньше минуты"},
		{name: "minutes round up", next: now.Add(20*time.Minute + time.Second), want: "21 мин"},
		{name: "hours", next: now.Add(2*time.Hour + 10*time.Minute), want: "2 ч 10 мин"},
		{name: "days", next: now.Add(29 * time.Hour), want: "1 д 5 ч"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := remainingLabel(tc.next, now); got != tc.want {
				t.Fatalf("remainingLabel() = %q, ожидалось %q", got, tc.want)
			}
		})
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
		"/api/trace/" + run.Meta.RunID + "?step=unknown&after=0",
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
		"TEST DATA", "Consolas", "nightly-review", "35 мин", "Расписание запусков", "Запуск: ", "sync-project-status", "weekly-report",
		"release-v0.3.0", "repair-failed-macos-build", "nightly-maintenance",
		"prepare-release-notes-with-a-deliberately-long-name", "tone-running", "tone-failed", "tone-succeeded", "skipped", "#preview",
		"Работа агента", "0/2 шагов", "1/2 шагов", "failed-nightly-cleanup", "previous-release", "За последние 24 часа", "За всё время",
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("preview не показывает %q", fragment)
		}
	}
	if strings.Contains(body, "Прогресс workflow и live-вывод") || strings.Contains(body, "<h1") || strings.Contains(body, "fake data") {
		t.Fatal("preview снова показывает старый заголовок, subtitle или неоформленную тестовую метку")
	}
	if strings.Contains(body, "Ближайший") || strings.Contains(body, "через 35 мин") {
		t.Fatal("пилюля ближайшего запуска снова содержит лишние пояснения")
	}
	if strings.Contains(body, "Будущие запуски") || strings.Contains(body, "schedule-eyebrow") {
		t.Fatal("панель снова показывает двухстрочный заголовок")
	}
	if strings.Contains(body, `data-refresh="3"`) {
		t.Fatal("статичный preview неожиданно начал polling")
	}
	live := httptest.NewRecorder()
	Handler(filepath.Join(t.TempDir(), "missing")).ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(live.Body.String(), "Запусков пока нет") || !strings.Contains(live.Body.String(), `data-refresh="3"`) {
		t.Fatal("пустой live dashboard не показывает подсказку или polling")
	}
}

// TestSortNodesNewestFirst фиксирует единый временной порядок без статусных
// секций: успешный новый workflow не должен оказаться ниже старого активного.
func TestSortNodesNewestFirst(t *testing.T) {
	old := &runNode{ID: "old-running", activityAt: time.Now().Add(-time.Hour)}
	recent := &runNode{ID: "recent-succeeded", activityAt: time.Now()}
	roots := []*runNode{old, recent}
	sortNodes(roots)
	if roots[0] != recent || roots[1] != old {
		t.Fatalf("workflow отсортированы не по времени: %+v", roots)
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
