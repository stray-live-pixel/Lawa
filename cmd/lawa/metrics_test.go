package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// CLI читает настоящий заказ, но не вызывает зависимости Codex и не меняет
// team.json. Повтор без --at должен печатать побайтно одинаковый JSON.
func TestMetricsCLIReadOnly(t *testing.T) {
	root := t.TempDir()
	s, err := runstore.Create(root, runstore.Input{Order: true, Team: true, CWD: t.TempDir(), Task: "Задача", WorkflowJSON: []byte(`{"id":"office","characters":{"boss":{"name":"Босс","history":"Опыт","instructions":"Работай"}},"steps":[{"id":"boss","type":"agent","character":"boss","prompt":"Работай","dependsOn":[]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, s.Meta.RunID, "team.json")
	before, _ := os.ReadFile(path)
	var out, again bytes.Buffer
	args := []string{"metrics", s.Meta.RunID, "--root", root}
	if err := executeContext(t.Context(), args, &out, &out, dependencies{}); err != nil {
		t.Fatal(err)
	}
	if err := executeContext(t.Context(), args, &again, &again, dependencies{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != again.String() {
		t.Fatal("отчёт меняется без новых данных")
	}
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report["runId"] != s.Meta.RunID || report["price"] != nil {
		t.Fatal(report)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("отчёт изменил team.json")
	}
	for _, tail := range [][]string{{"--at"}, {"--at", "bad"}, {"--at", "2026-09-21T00:00:00Z", "--at", "2026-09-21T00:00:00Z"}, {"--unknown"}} {
		if err := metricsCommand(append(args[1:], tail...), &bytes.Buffer{}, dependencies{}); err == nil {
			t.Fatal(tail)
		}
	}
	if !strings.Contains(help, "lawa metrics") || !strings.Contains(skillInstruction, "lawa metrics") {
		t.Fatal("CLI не описан")
	}
}
