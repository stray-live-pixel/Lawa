//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// agentObservabilityRun создаёт два законченных прохода self-loop и достигает
// maxVisits при третьей активации. Второй стартовый шаг остаётся Pending: это
// штатная история terminal outcome, а не незаписанное событие исполнителя.
func agentObservabilityRun(t *testing.T) (string, runstore.Snapshot, runstore.Visit, runstore.Visit) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runs")
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(`{
  "version": 2,
  "id": "observe-agent-graph",
  "start": ["loop", "unused"],
  "steps": [
    {"id":"loop","type":"agent","prompt":"Проверь","after":[],"maxVisits":2,"onLimit":"failed","decisions":{
      "again":{"to":["loop"]},"done":{"finish":"succeeded"}}},
    {"id":"unused","type":"agent","prompt":"Не запускать","after":[]}
  ]
}`),
		Task: "Проверить наблюдаемость agent graph", CWD: t.TempDir(),
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
	first := snapshot.Meta.Visits[0]
	finishAgentObservabilityVisit(t, run, first, "первый проход")
	advanced, err := run.AdvanceAgentGraph()
	if err != nil || len(advanced.CreatedVisits) != 1 {
		t.Fatalf("не создан второй проход: %+v, %v", advanced, err)
	}
	second := advanced.CreatedVisits[0]
	finishAgentObservabilityVisit(t, run, second, "второй проход")
	terminal, err := run.AdvanceAgentGraph()
	if err != nil || terminal.Snapshot.Meta.RunState != runstore.RunFailed {
		t.Fatalf("maxVisits не завершил run: %+v, %v", terminal, err)
	}
	return root, terminal.Snapshot, first, second
}

// finishAgentObservabilityVisit повторяет durable-порядок coordinator: reserve,
// связь с чатом, turn, решение, terminal metadata и только затем событие. Такой
// порядок важен для отдельной проверки, что logs --follow дожидается JSONL.
func finishAgentObservabilityVisit(t *testing.T, run *runstore.LockedRun, visit runstore.Visit, message string) {
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
	if _, err := run.CommitDecision(visit.VisitID, threadID, turnID, "again", "Нужна ещё проверка", "call-"+visit.VisitID); err != nil {
		t.Fatal(err)
	}
	if err := run.UpdateVisit(visit.VisitID, scheduler.Succeeded, threadID, ""); err != nil {
		t.Fatal(err)
	}
	if err := run.AppendEvent(runstore.RuntimeEvent{
		VisitID: visit.VisitID, StepID: visit.StepID, ThreadID: threadID, TurnID: turnID,
		Kind: "visit_state", State: string(scheduler.Succeeded), Message: message,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAgentStatusShowsVisitsRoutesAndLimit проверяет операторский снимок v4:
// повторные проходы не схлопываются, а причина остановки и непройденные routes
// читаются из durable истории, а не реконструируются из финального текста агента.
func TestAgentStatusShowsVisitsRoutesAndLimit(t *testing.T) {
	root, snapshot, first, second := agentObservabilityRun(t)
	var output bytes.Buffer
	if err := statusCommand([]string{snapshot.Meta.RunID, "--root", root}, &output, dependencies{}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, fragment := range []string{
		"runState: failed",
		"stopVisit: " + second.VisitID,
		"limit: step=loop, iteration=3, trigger=decision:again <- " + second.VisitID,
		"причина: ",
		"loop#1 [" + first.VisitID + "]: succeeded",
		"loop#2 [" + second.VisitID + "]: succeeded",
		"iteration: 1",
		"iteration: 2",
		"limit: maxVisits=2, onLimit=failed",
		"routes: again → loop; done → finish:succeeded",
		"решение: again, applied=true",
		"решение: again, applied=false",
		"переход: loop",
		"объяснение: Нужна ещё проверка",
		"skipped: done",
		"сообщение: первый проход",
		"unused#1 [",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("status не содержит %q:\n%s", fragment, text)
		}
	}
}

// TestAgentLogsFilterByStepAndVisit фиксирует две разные адресации: step-id
// собирает всю историю логического шага, а --visit оставляет ровно один проход.
// Известный, но ещё не посещённый шаг является корректным пустым фильтром.
func TestAgentLogsFilterByStepAndVisit(t *testing.T) {
	root, snapshot, first, second := agentObservabilityRun(t)
	deps := dependencies{}

	var byStep bytes.Buffer
	if err := logsCommand(t.Context(), []string{snapshot.Meta.RunID, "loop", "--root", root}, &byStep, deps); err != nil {
		t.Fatal(err)
	}
	if text := byStep.String(); !strings.Contains(text, "visit="+first.VisitID) || !strings.Contains(text, "visit="+second.VisitID) {
		t.Fatalf("фильтр шага потерял проход: %q", text)
	}

	var byVisit bytes.Buffer
	if err := logsCommand(t.Context(), []string{snapshot.Meta.RunID, "--visit", second.VisitID, "--root", root}, &byVisit, deps); err != nil {
		t.Fatal(err)
	}
	if text := byVisit.String(); strings.Contains(text, "visit="+first.VisitID) || !strings.Contains(text, "visit="+second.VisitID) {
		t.Fatalf("точный фильтр visit смешал проходы: %q", text)
	}

	var unvisited bytes.Buffer
	if err := logsCommand(t.Context(), []string{snapshot.Meta.RunID, "unused", "--root", root}, &unvisited, deps); err != nil || unvisited.Len() != 0 {
		t.Fatalf("известный шаг без событий не стал пустым фильтром: %q, %v", unvisited.String(), err)
	}
}

// TestAgentLogsRejectInvalidVisitFilters покрывает строгий CLI-контракт. VisitID
// нельзя угадывать, смешивать со StepID или применять к legacy metadata.
func TestAgentLogsRejectInvalidVisitFilters(t *testing.T) {
	root, snapshot, _, visit := agentObservabilityRun(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"неизвестный visit", []string{snapshot.Meta.RunID, "--visit", "missing", "--root", root}, "не содержит посещение"},
		{"пустой visit", []string{snapshot.Meta.RunID, "--visit=", "--root", root}, "visit-id должен быть непустым"},
		{"повтор visit", []string{snapshot.Meta.RunID, "--visit", visit.VisitID, "--visit", visit.VisitID, "--root", root}, "параметр --visit повторён"},
		{"visit и step", []string{snapshot.Meta.RunID, "loop", "--visit", visit.VisitID, "--root", root}, "взаимоисключающие"},
		{"неизвестный step", []string{snapshot.Meta.RunID, "missing", "--root", root}, "не содержит шаг"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := logsCommand(t.Context(), tc.args, io.Discard, dependencies{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("получена ошибка %v, ожидался фрагмент %q", err, tc.want)
			}
		})
	}

	legacyRoot := filepath.Join(t.TempDir(), "legacy")
	legacy, err := runstore.Create(legacyRoot, runstore.Input{
		WorkflowJSON: []byte(`{"id":"legacy","steps":[{"id":"step","type":"agent","prompt":"Работай","dependsOn":[]}]}`),
		Task:         "Legacy", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = logsCommand(t.Context(), []string{legacy.Meta.RunID, "--visit", visit.VisitID, "--root", legacyRoot}, io.Discard, dependencies{})
	if err == nil || !strings.Contains(err.Error(), "только для workflow version=2") {
		t.Fatalf("legacy run принял --visit: %v", err)
	}
}

// TestAgentLogsFollowDrainsVisitEventAndIgnoresUnstarted проверяет три границы
// завершения: terminal RunState не опережает финальный JSONL, но Pending и
// зарезервированный Starting без thread/turn не требуют несуществующих событий.
func TestAgentLogsFollowDrainsVisitEventAndIgnoresUnstarted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	snapshot, err := runstore.Create(root, runstore.Input{
		WorkflowJSON: []byte(`{
  "version":2,"id":"follow-v2","start":["finish","pending","ambiguous"],"steps":[
    {"id":"finish","type":"agent","prompt":"Заверши","after":[],"decisions":{"done":{"finish":"succeeded"}}},
    {"id":"pending","type":"agent","prompt":"Не запускать","after":[]},
    {"id":"ambiguous","type":"agent","prompt":"Только зарезервировать","after":[]}
  ]}`),
		Task: "Проверить follow v2", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	visit := snapshot.Meta.Visits[0]
	threadID, turnID := "thread-finish", "turn-finish"
	if err = run.ReserveVisits([]string{visit.VisitID, snapshot.Meta.Visits[2].VisitID}); err == nil {
		err = run.UpdateVisit(visit.VisitID, scheduler.Unknown, threadID, "")
	}
	if err == nil {
		err = run.SetVisitTurn(visit.VisitID, turnID)
	}
	if err == nil {
		err = run.AppendEvent(runstore.RuntimeEvent{
			VisitID: visit.VisitID, StepID: visit.StepID, ThreadID: threadID, TurnID: turnID, Kind: "turn_bound",
		})
	}
	if err == nil {
		_, err = run.CommitDecision(visit.VisitID, threadID, turnID, "done", "Работа завершена", "call-finish")
	}
	if err == nil {
		err = run.UpdateVisit(visit.VisitID, scheduler.Succeeded, threadID, "")
	}
	if err == nil {
		_, err = run.AdvanceAgentGraph()
	}
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- logsCommand(ctx, []string{snapshot.Meta.RunID, "--root", root, "--follow"}, &output, dependencies{logsPollInterval: time.Millisecond})
	}()
	select {
	case followErr := <-done:
		t.Fatalf("follow завершился до visit_state: %v", followErr)
	case <-time.After(20 * time.Millisecond):
	}
	if err = run.AppendEvent(runstore.RuntimeEvent{
		VisitID: visit.VisitID, StepID: visit.StepID, ThreadID: threadID, TurnID: turnID,
		Kind: "visit_state", State: string(scheduler.Succeeded),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case followErr := <-done:
		if followErr != nil {
			t.Fatal(followErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow не завершился после финального visit_state")
	}
	text := output.String()
	if strings.Count(text, "turn_bound") != 1 || strings.Count(text, "visit_state") != 1 {
		t.Fatalf("follow повторил старое или потерял финальное событие: %q", text)
	}
}
