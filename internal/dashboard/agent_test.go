//go:build darwin || linux

package dashboard

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// createAgentDashboardRun строит историю с двумя проходами self-loop и
// terminal maxVisits. Независимый стартовый visit остаётся Pending, чтобы
// dashboard показывал фактическую историю, а не выдумывал skipped-состояние.
func createAgentDashboardRun(t *testing.T) (string, runstore.Snapshot) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runs")
	snapshot, err := runstore.CreateAgentGraph(root, runstore.Input{
		WorkflowJSON: []byte(`{
  "version":2,"id":"dashboard-v2","start":["loop","unused"],"steps":[
    {"id":"loop","type":"agent","prompt":"Проверь","after":[],"maxVisits":2,"onLimit":"failed","decisions":{
      "again":{"to":["loop"]},"done":{"finish":"succeeded"}}},
    {"id":"unused","type":"agent","prompt":"Не запускать","after":[]}
  ]}`),
		Task: "Показать историю dashboard", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := run.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	finishDashboardVisit(t, run, snapshot.Meta.Visits[0], "первый итог", "вывод первого прохода")
	advanced, err := run.AdvanceAgentGraph()
	if err != nil || len(advanced.CreatedVisits) != 1 {
		t.Fatalf("не создан повторный visit: %+v, %v", advanced, err)
	}
	finishDashboardVisit(t, run, advanced.CreatedVisits[0], "второй итог", "вывод второго прохода")
	terminal, err := run.AdvanceAgentGraph()
	if err != nil || terminal.Snapshot.Meta.RunState != runstore.RunFailed {
		t.Fatalf("run не остановлен лимитом: %+v, %v", terminal, err)
	}
	for _, visit := range terminal.Snapshot.Meta.Visits {
		if visit.StepID != "loop" {
			continue
		}
		if err = os.WriteFile(filepath.Join(root, snapshot.Meta.RunID, "memory", visit.VisitID+".md"), []byte("память "+visit.VisitID), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, terminal.Snapshot
}

// finishDashboardVisit сохраняет раздельные summary/content для каждого
// прохода. Тест гидратации благодаря этому обнаружит свёртку по StepID вместо
// VisitID даже тогда, когда HTML-ярлыки сами по себе выглядят корректно.
func finishDashboardVisit(t *testing.T, run *runstore.LockedRun, visit runstore.Visit, summary, content string) {
	t.Helper()
	threadID, turnID := "thread-"+visit.VisitID, "turn-"+visit.VisitID
	if err := run.ReserveVisits([]string{visit.VisitID}); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(visit.VisitID, scheduler.Unknown, threadID, ""); err != nil {
		t.Fatal(err)
	}
	if err := run.SetVisitTurn(visit.VisitID, turnID); err != nil {
		t.Fatal(err)
	}
	if err := run.AppendEvent(runstore.RuntimeEvent{
		VisitID: visit.VisitID, StepID: visit.StepID, ThreadID: threadID, TurnID: turnID,
		Kind: "agent_message_delta", ItemID: "message", ItemType: "agentMessage", Content: content,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.CommitDecision(visit.VisitID, threadID, turnID, "again", "Повторить проверку", "call-"+visit.VisitID); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(visit.VisitID, scheduler.Succeeded, threadID, ""); err != nil {
		t.Fatal(err)
	}
	if err := run.AppendEvent(runstore.RuntimeEvent{
		VisitID: visit.VisitID, StepID: visit.StepID, ThreadID: threadID, TurnID: turnID,
		Kind: "visit_state", State: string(scheduler.Succeeded), Message: summary,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAgentDashboardShowsDurableVisitHistory проверяет карточки, уникальные
// inspector-ключи и ссылки. Все объяснения берутся из metadata v4; dashboard не
// создаёт ложных листов для невыбранных routes и не смешивает проходы цикла.
func TestAgentDashboardShowsDurableVisitHistory(t *testing.T) {
	root, snapshot := createAgentDashboardRun(t)
	first, pending, second := snapshot.Meta.Visits[0], snapshot.Meta.Visits[1], snapshot.Meta.Visits[2]
	recorder := httptest.NewRecorder()
	Handler(root).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?period=all&view=all", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("dashboard вернул status %d", recorder.Code)
	}
	html := recorder.Body.String()
	for _, fragment := range []string{
		"dashboard-v2", "Workflow · failed", "Посещения", "2 из 3 завершено",
		"loop#1", "loop#2", "unused#1", "Причина остановки", "Достигнут лимит",
		"loop · iteration=3 · decision:again ← " + second.VisitID,
		"Причина запуска", "decision:again ← " + first.VisitID,
		"Решение", "again · applied=true", "again · applied=false",
		"Объяснение решения", "Повторить проверку", "Остановившее посещение", second.VisitID,
		"Переход", "Пропущенные routes", "done", "maxVisits=2 · onLimit=failed",
		`data-inspector-select="step:` + snapshot.Meta.RunID + `:` + first.VisitID + `"`,
		`data-inspector-select="step:` + snapshot.Meta.RunID + `:` + second.VisitID + `"`,
		`/events/` + snapshot.Meta.RunID + `?visit=` + first.VisitID,
		`/api/trace/` + snapshot.Meta.RunID + `?visit=` + second.VisitID,
		`/memory/` + snapshot.Meta.RunID + `/` + first.VisitID,
	} {
		if !strings.Contains(html, fragment) {
			t.Errorf("dashboard не содержит %q", fragment)
		}
	}
	if strings.Contains(html, `step:`+snapshot.Meta.RunID+`:loop"`) ||
		!strings.Contains(html, `data-inspector-select="step:`+snapshot.Meta.RunID+`:`+pending.VisitID+`"`) {
		t.Fatal("повторы получили общий inspector key или фактический Pending visit потерян")
	}

	node := makeRunNode(root, snapshot)
	if err := hydrateRunNode(root, node, false); err != nil {
		t.Fatal(err)
	}
	byKey := make(map[string]stepNode, len(node.Steps))
	for _, step := range node.Steps {
		byKey[step.Key] = step
	}
	if byKey[first.VisitID].Message != "первый итог" || byKey[second.VisitID].Message != "второй итог" {
		t.Fatalf("сводки visits смешаны: first=%+v second=%+v", byKey[first.VisitID], byKey[second.VisitID])
	}
}

// TestAgentDashboardVisitEndpoints фиксирует exact visit scope для журнала,
// live-вывода и памяти. Позиционный аналог step остаётся логическим фильтром по
// обоим проходам, а пустой/двойной/чужой visit не раскрывает весь run.
func TestAgentDashboardVisitEndpoints(t *testing.T) {
	root, snapshot := createAgentDashboardRun(t)
	first, second := snapshot.Meta.Visits[0], snapshot.Meta.Visits[2]
	dashboard := Handler(root)

	request := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		dashboard.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}
	firstEvents := request("/events/" + snapshot.Meta.RunID + "?visit=" + first.VisitID)
	if body := firstEvents.Body.String(); firstEvents.Code != http.StatusOK || !strings.Contains(body, "первый итог") || strings.Contains(body, "второй итог") {
		t.Fatalf("visit-журнал смешан: status=%d body=%q", firstEvents.Code, body)
	}
	stepEvents := request("/events/" + snapshot.Meta.RunID + "?step=loop")
	if body := stepEvents.Body.String(); !strings.Contains(body, "первый итог") || !strings.Contains(body, "второй итог") {
		t.Fatalf("логический step-фильтр потерял проход: %q", body)
	}
	traceRecorder := request("/api/trace/" + snapshot.Meta.RunID + "?visit=" + second.VisitID + "&after=0")
	var trace traceResponse
	if err := json.Unmarshal(traceRecorder.Body.Bytes(), &trace); err != nil || traceRecorder.Code != http.StatusOK ||
		len(trace.Events) != 1 || trace.Events[0].Content != "вывод второго прохода" {
		t.Fatalf("visit trace неверен: status=%d trace=%+v err=%v", traceRecorder.Code, trace, err)
	}
	memory := request("/memory/" + snapshot.Meta.RunID + "/" + first.VisitID)
	if memory.Code != http.StatusOK || memory.Body.String() != "память "+first.VisitID {
		t.Fatalf("память visit недоступна: status=%d body=%q", memory.Code, memory.Body.String())
	}
	legacy := createRun(t, root, "legacy-neighbor", "")
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/events/" + snapshot.Meta.RunID + "?visit=", http.StatusBadRequest},
		{"/events/" + snapshot.Meta.RunID + "?visit=" + first.VisitID + "&step=loop", http.StatusBadRequest},
		{"/api/trace/" + snapshot.Meta.RunID + "?visit=missing&after=0", http.StatusNotFound},
		{"/events/" + legacy.Meta.RunID + "?visit=" + first.VisitID, http.StatusNotFound},
	} {
		response := request(tc.path)
		if response.Code != tc.want {
			t.Errorf("неверный scope %q получил status %d, ожидался %d", tc.path, response.Code, tc.want)
		}
	}
}

// TestAgentDashboardUsesRunStateAfterHandledFailure доказывает различие уровней:
// технический Failed уже породил after-visit, поэтому running workflow остаётся
// в активном представлении и не попадает в фильтр провалов.
func TestAgentDashboardUsesRunStateAfterHandledFailure(t *testing.T) {
	root := t.TempDir()
	snapshot, err := runstore.CreateAgentGraph(root, runstore.Input{
		WorkflowJSON: []byte(`{
  "version":2,"id":"handled-failure","start":["work"],"steps":[
    {"id":"work","type":"agent","prompt":"Работай","after":[]},
    {"id":"inspect","type":"agent","prompt":"Разбери результат","after":["work"]}
  ]}`),
		Task: "Обработать технический отказ", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	visit := snapshot.Meta.Visits[0]
	if err = run.ReserveVisits([]string{visit.VisitID}); err == nil {
		err = run.UpdateVisit(visit.VisitID, scheduler.Unknown, "thread-work", "")
	}
	if err == nil {
		err = run.SetVisitTurn(visit.VisitID, "turn-work")
	}
	if err == nil {
		err = run.UpdateVisit(visit.VisitID, scheduler.Failed, "thread-work", "команда завершилась с ошибкой")
	}
	if err == nil {
		_, err = run.AdvanceAgentGraph()
	}
	if closeErr := run.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	current, err := runstore.Load(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	node := makeRunNode(root, current)
	finalizeTree(node)
	if node.State != "running" || node.Tone != "running" || !node.HasUnfinished || node.HasFailed || failedTree(node) != nil {
		t.Fatalf("handled Failed visit подменил RunState: %+v", node)
	}
	active := httptest.NewRecorder()
	Handler(root).ServeHTTP(active, httptest.NewRequest(http.MethodGet, "/?period=all", nil))
	failed := httptest.NewRecorder()
	Handler(root).ServeHTTP(failed, httptest.NewRequest(http.MethodGet, "/?period=all&view=all&states=failed", nil))
	if !strings.Contains(active.Body.String(), "handled-failure") || strings.Contains(failed.Body.String(), "handled-failure") {
		t.Fatal("dashboard-фильтры не следуют авторитетному v4 RunState")
	}
}

// TestFailedTreeKeepsAgentVisitsOnlyForOwnFailure проверяет проекцию дерева:
// v4-родитель нужен как путь к упавшему ребёнку, но его успешные и активные
// visits не являются результатами фильтра «Сломавшиеся».
func TestFailedTreeKeepsAgentVisitsOnlyForOwnFailure(t *testing.T) {
	parent := &runNode{
		ID: "parent", State: "running", AgentGraph: true,
		Steps: []stepNode{
			{Key: "visit-done", ID: "work#1", State: "succeeded"},
			{Key: "visit-active", ID: "check#1", State: "running", Active: true},
		},
	}
	parent.ActiveSteps = []stepNode{parent.Steps[1]}
	parent.Children = []*runNode{{ID: "failed-child", State: "failed"}}
	finalizeTree(parent)
	projected := failedTree(parent)
	if projected == nil || len(projected.Children) != 1 || projected.Children[0].ID != "failed-child" || len(projected.Steps) != 0 {
		t.Fatalf("failed-проекция смешала visits исправного предка: %+v", projected)
	}
}
