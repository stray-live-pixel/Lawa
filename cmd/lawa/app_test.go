package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
