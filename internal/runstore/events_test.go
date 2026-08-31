package runstore

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// TestRuntimeEventsAndSummary проверяет durable журнал, сохранение turn и
// свёртку процесса, которую используют status и dashboard.
func TestRuntimeEventsAndSummary(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Create(root, testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	stepID := snapshot.Meta.Steps[0].ID
	run, err := OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Reserve([]string{stepID}); err == nil {
		err = run.Update(stepID, scheduler.Unknown, "thread-1")
	}
	if err == nil {
		err = run.SetTurn(stepID, "turn-1")
	}
	started := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	if err == nil {
		err = run.AppendEvent(RuntimeEvent{Time: started, StepID: stepID, ThreadID: "thread-1", TurnID: "turn-1", Kind: "process_started", PID: 4242})
	}
	if err == nil {
		err = run.AppendEvent(RuntimeEvent{Time: started.Add(time.Second), StepID: stepID, ThreadID: "thread-1", TurnID: "turn-1", Kind: "item_completed", ItemType: "agentMessage", Message: "  готово\nбез сырого payload  "})
	}
	exitCode := 0
	if err == nil {
		err = run.AppendEvent(RuntimeEvent{Time: started.Add(2 * time.Second), StepID: stepID, Kind: "process_exited", PID: 4242, ExitCode: &exitCode})
	}
	if closeErr := run.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(root, snapshot.Meta.RunID)
	if err != nil || loaded.Meta.Steps[0].TurnID != "turn-1" {
		t.Fatalf("turn не сохранён: %+v, %v", loaded.Meta.Steps[0], err)
	}
	events, err := ReadEvents(root, snapshot.Meta.RunID)
	if err != nil || len(events) != 3 || events[1].Message != "готово без сырого payload" {
		t.Fatalf("журнал не прочитан или не нормализован: %+v, %v", events, err)
	}
	summary := SummarizeEvents(events)[stepID]
	if summary.PID != 0 || summary.ExitCode == nil || *summary.ExitCode != 0 || summary.TurnID != "turn-1" || summary.Message != events[1].Message {
		t.Fatalf("неверная сводка событий: %+v", summary)
	}
	info, err := os.Stat(filepath.Join(root, snapshot.Meta.RunID, eventsFilename))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("неверные права журнала: %v, %v", info, err)
	}
}

// TestHistoricalAppNativeRunIsReadOnly воспроизводит выпущенный формат v2 с
// marker создания Desktop-задачи. Чтение остаётся доступно, writer запрещён.
func TestHistoricalAppNativeRunIsReadOnly(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Create(root, testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	meta := snapshot.Meta
	meta.Version, meta.InitiatorThreadID = 2, "historical-controller"
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, snapshot.Meta.RunID)
	if err = os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "app-create-"+snapshot.Meta.Steps[0].ThreadID)
	if err = os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadForDashboard(root, snapshot.Meta.RunID)
	if err != nil || !loaded.HistoricalAppNative {
		t.Fatalf("исторический run не распознан: %+v, %v", loaded, err)
	}
	if _, err = OpenLocked(root, snapshot.Meta.RunID); !errors.Is(err, ErrHistoricalAppNative) || !strings.Contains(err.Error(), "только для чтения") {
		t.Fatalf("исторический run открыт координатором: %v", err)
	}
}

// TestRuntimeEventRejectsUnboundedUsage не позволяет повреждённому или будущему
// журналу обходить ограничение размера через карту числовых счётчиков.
func TestRuntimeEventRejectsUnboundedUsage(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Create(root, testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	usage := make(map[string]int64, 65)
	for index := 0; index < 65; index++ {
		usage[fmt.Sprintf("counter.%d", index)] = int64(index)
	}
	if err = run.AppendEvent(RuntimeEvent{Kind: "token_usage_updated", Usage: usage}); err == nil || !strings.Contains(err.Error(), "слишком много") {
		t.Fatalf("неограниченный usage принят: %v", err)
	}
	_ = run.Close()
}
