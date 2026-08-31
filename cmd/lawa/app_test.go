package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/appdriver"
	"github.com/stray-live-pixel/Lawa/internal/codex"
)

func TestAppCommandsDriveRunWithoutCodexServer(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{
  "id":"app-cli",
  "steps":[{"id":"work","type":"agent","prompt":"do work","dependsOn":[]}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := productionDependencies()
	deps.renderer = successfulCLIRenderer()
	// Если app-run по ошибке вернётся к legacy пути, тест упадёт до создания run.
	deps.check = func(context.Context, codex.Connection) error {
		t.Fatal("app-run не должен открывать app-server")
		return nil
	}
	deps.userHomeDir = func() (string, error) { return t.TempDir(), nil }

	var output bytes.Buffer
	err := executeContext(context.Background(), []string{
		"app-run", workflowPath, "--cwd", cwd, "--task", "user task",
		"--initiator-thread-id", "parent-task", "--root", root,
	}, &output, &output, deps)
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		RunID string `json:"runId"`
	}
	if err = json.Unmarshal(bytes.TrimSpace(output.Bytes()), &started); err != nil || started.RunID == "" {
		t.Fatalf("app-run output %q: %v", output.String(), err)
	}

	action := appCommandAction(t, deps, root, started.RunID)
	if action.Kind != "launch" || action.Launch == nil || action.Launch.StepID != "work" {
		t.Fatalf("app-next = %#v", action)
	}
	if !appClaimResult(t, deps, root, started.RunID, "work") {
		t.Fatal("первая попытка должна получить claim")
	}
	if appClaimResult(t, deps, root, started.RunID, "work") {
		t.Fatal("повтор без подтверждённого reset не должен получить claim")
	}
	var resetOutput bytes.Buffer
	err = executeContext(context.Background(), []string{
		"app-reset-claim", started.RunID, "--root", root, "--step", "work", "--confirm-reset", "other",
	}, &resetOutput, &resetOutput, deps)
	if err == nil || !strings.Contains(err.Error(), "дубликат") {
		t.Fatalf("нет защиты от случайного reset: %v", err)
	}
	runAppCommand(t, deps, "app-reset-claim", started.RunID, "--root", root, "--step", "work", "--confirm-reset", "work")
	if !appClaimResult(t, deps, root, started.RunID, "work") {
		t.Fatal("подтверждённый reset не разрешил одну новую попытку")
	}
	runAppCommand(t, deps, "app-bind", started.RunID, "--root", root, "--step", "work", "--thread-id", "child-task")
	runAppCommand(t, deps, "app-update", started.RunID, "--root", root, "--step", "work", "--state", "running", "--revision", "0")
	resultPath := filepath.Join(t.TempDir(), "result.md")
	if err = os.WriteFile(resultPath, []byte("finished in Codex App"), 0o600); err != nil {
		t.Fatal(err)
	}
	runAppCommand(t, deps, "app-update", started.RunID, "--root", root, "--step", "work", "--state", "succeeded", "--revision", "1", "--result-file", resultPath)
	if done := appCommandAction(t, deps, root, started.RunID); done.Kind != "complete" {
		t.Fatalf("финальное действие = %#v", done)
	}
}

// TestAppSeriesAdvancesWithoutLongLivedProcess воспроизводит завершение
// управляющего turn между повторами. Каждый app-series-next заново открывает
// серию, возвращает текущий run без дубля и создаёт следующий только после
// подтверждённого успеха предыдущего.
func TestAppSeriesAdvancesWithoutLongLivedProcess(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{
  "id":"app-series",
  "steps":[{"id":"work","type":"agent","prompt":"do work","dependsOn":[]}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	current := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	deps := productionDependencies()
	deps.now = func() time.Time { return current }
	deps.renderer = successfulCLIRenderer()
	deps.userHomeDir = func() (string, error) { return t.TempDir(), nil }

	var output bytes.Buffer
	err := executeContext(context.Background(), []string{
		"app-run", workflowPath, "--cwd", cwd, "--task", "repeat task",
		"--initiator-thread-id", "controller", "--root", root,
		"--repeat", "after", "--repeat-delay", "1h", "--max-runs", "2",
	}, &output, &output, deps)
	if err != nil {
		t.Fatal(err)
	}
	var first appSeriesAction
	if err = json.Unmarshal(bytes.TrimSpace(output.Bytes()), &first); err != nil || first.Kind != "run" || first.SeriesID == "" || first.RunID == "" {
		t.Fatalf("первое действие серии %q: %+v, %v", output.String(), first, err)
	}
	if same := appSeriesNextAction(t, deps, root, first.SeriesID); same.Kind != "run" || same.RunID != first.RunID {
		t.Fatalf("активный run был заменён: %+v", same)
	}
	finishSingleStepAppRun(t, deps, root, first.RunID, "first-task", "first result")
	waiting := appSeriesNextAction(t, deps, root, first.SeriesID)
	if waiting.Kind != "wait" || waiting.RunID != "" || waiting.NextRunAt == nil || !waiting.NextRunAt.Equal(current.Add(time.Hour)) {
		t.Fatalf("после первого run = %+v", waiting)
	}
	if repeated := appSeriesNextAction(t, deps, root, first.SeriesID); repeated.Kind != "wait" || repeated.NextRunAt == nil || !repeated.NextRunAt.Equal(*waiting.NextRunAt) {
		t.Fatalf("ранний heartbeat изменил расписание: %+v", repeated)
	}
	current = current.Add(time.Hour)
	second := appSeriesNextAction(t, deps, root, first.SeriesID)
	if second.Kind != "run" || second.RunID == "" || second.RunID == first.RunID {
		t.Fatalf("второй run не создан: %+v", second)
	}
	finishSingleStepAppRun(t, deps, root, second.RunID, "second-task", "second result")
	if done := appSeriesNextAction(t, deps, root, first.SeriesID); done.Kind != "complete" || done.State != "completed" {
		t.Fatalf("серия не завершилась по лимиту: %+v", done)
	}
}

// TestAppCronSeriesWaitsBeforeFirstRun не позволяет app-native режиму незаметно
// изменить календарную семантику legacy: cron ждёт первую будущую точку.
func TestAppCronSeriesWaitsBeforeFirstRun(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{"id":"cron","steps":[{"id":"work","type":"agent","prompt":"work","dependsOn":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := productionDependencies()
	deps.now = func() time.Time { return time.Date(2026, 8, 31, 6, 59, 0, 0, time.UTC) }
	deps.renderer = successfulCLIRenderer()
	var output bytes.Buffer
	if err := executeContext(context.Background(), []string{
		"app-run", workflowPath, "--cwd", cwd, "--task", "cron task",
		"--initiator-thread-id", "controller", "--root", root,
		"--repeat", "cron", "--cron", "0 10 * * *", "--timezone", "Europe/Moscow",
	}, &output, &output, deps); err != nil {
		t.Fatal(err)
	}
	var action appSeriesAction
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &action); err != nil || action.Kind != "wait" || action.RunID != "" || action.NextRunAt == nil {
		t.Fatalf("первое cron-действие %q: %+v, %v", output.String(), action, err)
	}
}

func TestAppSeriesFailRequiresCurrentRunIdentity(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{"id":"failure","steps":[{"id":"work","type":"agent","prompt":"work","dependsOn":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := productionDependencies()
	deps.renderer = successfulCLIRenderer()
	var output bytes.Buffer
	if err := executeContext(context.Background(), []string{
		"app-run", workflowPath, "--cwd", cwd, "--task", "task", "--initiator-thread-id", "controller",
		"--root", root, "--repeat", "immediate", "--max-runs", "2",
	}, &output, &output, deps); err != nil {
		t.Fatal(err)
	}
	var started appSeriesAction
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &started); err != nil {
		t.Fatal(err)
	}
	reason := filepath.Join(t.TempDir(), "reason.txt")
	if err := os.WriteFile(reason, []byte("кубик завершился failed"), 0o600); err != nil {
		t.Fatal(err)
	}
	var failed bytes.Buffer
	err := executeContext(context.Background(), []string{
		"app-series-fail", started.SeriesID, "--run", "wrong-run", "--reason-file", reason, "--root", root,
	}, &failed, &failed, deps)
	if err == nil || !strings.Contains(err.Error(), started.RunID) {
		t.Fatalf("чужой run принят: %v", err)
	}
	launch := appCommandAction(t, deps, root, started.RunID)
	if launch.Launch == nil || !appClaimResult(t, deps, root, started.RunID, launch.Launch.StepID) {
		t.Fatalf("не удалось подготовить failed run: %#v", launch)
	}
	runAppCommand(t, deps, "app-bind", started.RunID, "--root", root, "--step", "work", "--thread-id", "failed-task")
	runAppCommand(t, deps, "app-update", started.RunID, "--root", root, "--step", "work", "--state", "failed", "--revision", "0")
	failed.Reset()
	if err = executeContext(context.Background(), []string{
		"app-series-fail", started.SeriesID, "--run", started.RunID, "--reason-file", reason, "--root", root,
	}, &failed, &failed, deps); err != nil {
		t.Fatal(err)
	}
	var action appSeriesAction
	if err = json.Unmarshal(bytes.TrimSpace(failed.Bytes()), &action); err != nil || action.Kind != "failed" || action.State != "failed" {
		t.Fatalf("неверный terminal серии %q: %+v, %v", failed.String(), action, err)
	}
	if repeated := appSeriesNextAction(t, deps, root, started.SeriesID); repeated.Kind != "failed" {
		t.Fatalf("failed-серия снова открылась: %+v", repeated)
	}
}

func TestAppRunRejectsUnsupportedSpeedBeforeCreatingRun(t *testing.T) {
	root, cwd := filepath.Join(t.TempDir(), "runs"), t.TempDir()
	workflowPath := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{
  "id":"fast",
  "steps":[{"id":"work","type":"agent","prompt":"work","dependsOn":[],"speed":"fast"}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := productionDependencies()
	deps.renderer = successfulCLIRenderer()
	var output bytes.Buffer
	err := executeContext(context.Background(), []string{
		"app-run", workflowPath, "--cwd", cwd, "--task", "task",
		"--initiator-thread-id", "parent", "--root", root,
	}, &output, &output, deps)
	if err == nil || !strings.Contains(err.Error(), "service tier") {
		t.Fatalf("ожидалась явная ошибка speed, получено %v", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("run не должен создаваться: %v", statErr)
	}
}

func TestAppContinueClaimCommandIsDurable(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{"id":"resume","steps":[{"id":"work","type":"agent","prompt":"work","dependsOn":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := productionDependencies()
	deps.renderer = successfulCLIRenderer()
	var output bytes.Buffer
	if err := executeContext(context.Background(), []string{
		"app-run", workflowPath, "--cwd", cwd, "--task", "task", "--initiator-thread-id", "controller", "--root", root,
	}, &output, &output, deps); err != nil {
		t.Fatal(err)
	}
	var started struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &started); err != nil {
		t.Fatal(err)
	}
	_ = appCommandAction(t, deps, root, started.RunID)
	if !appClaimResult(t, deps, root, started.RunID, "work") {
		t.Fatal("нет creation claim")
	}
	runAppCommand(t, deps, "app-bind", started.RunID, "--root", root, "--step", "work", "--thread-id", "task")
	runAppCommand(t, deps, "app-update", started.RunID, "--root", root, "--step", "work", "--state", "cancelled", "--revision", "0")
	claim := func() bool {
		t.Helper()
		var claimOutput bytes.Buffer
		if err := executeContext(context.Background(), []string{
			"app-continue-claim", started.RunID, "--root", root, "--step", "work", "--turn-id", "interrupted-turn",
		}, &claimOutput, &claimOutput, deps); err != nil {
			t.Fatal(err)
		}
		var result struct {
			MayContinue bool `json:"mayContinue"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(claimOutput.Bytes()), &result); err != nil {
			t.Fatal(err)
		}
		return result.MayContinue
	}
	if !claim() || claim() {
		t.Fatal("continue claim не ограничил внешнюю отправку одним разом")
	}
}

func appCommandAction(t *testing.T, deps dependencies, root, runID string) appdriver.Action {
	t.Helper()
	var output bytes.Buffer
	if err := executeContext(context.Background(), []string{"app-next", runID, "--root", root}, &output, &output, deps); err != nil {
		t.Fatal(err)
	}
	var action appdriver.Action
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &action); err != nil {
		t.Fatalf("прочитать app-next %q: %v", output.String(), err)
	}
	return action
}

func appClaimResult(t *testing.T, deps dependencies, root, runID, stepID string) bool {
	t.Helper()
	var output bytes.Buffer
	if err := executeContext(context.Background(), []string{
		"app-claim", runID, "--root", root, "--step", stepID,
	}, &output, &output, deps); err != nil {
		t.Fatal(err)
	}
	var result struct {
		MayCreate bool `json:"mayCreate"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &result); err != nil {
		t.Fatalf("прочитать app-claim %q: %v", output.String(), err)
	}
	return result.MayCreate
}

func runAppCommand(t *testing.T, deps dependencies, args ...string) {
	t.Helper()
	var output bytes.Buffer
	if err := executeContext(context.Background(), args, &output, &output, deps); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	if strings.TrimSpace(output.String()) != "ok" {
		t.Fatalf("%v output = %q", args, output.String())
	}
}

func appSeriesNextAction(t *testing.T, deps dependencies, root, seriesID string) appSeriesAction {
	t.Helper()
	var output bytes.Buffer
	if err := executeContext(context.Background(), []string{"app-series-next", seriesID, "--root", root}, &output, &output, deps); err != nil {
		t.Fatal(err)
	}
	var action appSeriesAction
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &action); err != nil {
		t.Fatalf("прочитать app-series-next %q: %v", output.String(), err)
	}
	return action
}

func finishSingleStepAppRun(t *testing.T, deps dependencies, root, runID, taskID, result string) {
	t.Helper()
	action := appCommandAction(t, deps, root, runID)
	if action.Launch == nil || action.Launch.StepID != "work" || action.Launch.CWD == "" {
		t.Fatalf("неверный launch app-серии: %#v", action)
	}
	if !strings.Contains(action.Launch.Prompt, "Не запускай `lawa run`") {
		t.Fatalf("launch не защищает вложенный workflow: %q", action.Launch.Prompt)
	}
	if !appClaimResult(t, deps, root, runID, "work") {
		t.Fatal("app-серия не получила claim")
	}
	runAppCommand(t, deps, "app-bind", runID, "--root", root, "--step", "work", "--thread-id", taskID)
	runAppCommand(t, deps, "app-update", runID, "--root", root, "--step", "work", "--state", "running", "--revision", "0")
	resultPath := filepath.Join(t.TempDir(), "result.md")
	if err := os.WriteFile(resultPath, []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
	runAppCommand(t, deps, "app-update", runID, "--root", root, "--step", "work", "--state", "succeeded", "--revision", "1", "--result-file", resultPath)
}
