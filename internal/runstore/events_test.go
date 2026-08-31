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

// TestFormatEventEscapesTerminalControls защищает CLI от ANSI/OSC injection:
// данные остаются различимыми, но ни один управляющий байт не достигает терминала.
func TestFormatEventEscapesTerminalControls(t *testing.T) {
	event := RuntimeEvent{
		Time: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), Kind: "error\x1b[2J",
		StepID: "step\x07", ThreadID: "thread\u009b", Message: "первая\nвторая",
		Usage: map[string]int64{"tokens\x1b]52;c;secret\x07": 1},
	}
	formatted := FormatEvent(event)
	for _, forbidden := range []string{"\x1b", "\x07", "\u009b", "\n"} {
		if strings.Contains(formatted, forbidden) {
			t.Fatalf("FormatEvent оставил управляющий символ %q: %q", forbidden, formatted)
		}
	}
	for _, visible := range []string{`\u001B[2J`, `step\u0007`, `thread\u009B`, `первая\u000Aвторая`, `\u001B]52;c;secret\u0007`} {
		if !strings.Contains(formatted, visible) {
			t.Fatalf("FormatEvent потерял видимое экранирование %q: %q", visible, formatted)
		}
	}
}

// TestReadEventsAfterWaitsForCompleteLine проверяет границу concurrent append:
// половина JSON не становится ошибкой и не двигает cursor, а после дописывания
// той же строки событие возвращается ровно один раз.
func TestReadEventsAfterWaitsForCompleteLine(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Create(root, testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	stepID := snapshot.Meta.Steps[0].ID
	run, err := OpenLocked(root, snapshot.Meta.RunID)
	if err == nil {
		err = run.AppendEvent(RuntimeEvent{StepID: stepID, Kind: "process_started", PID: 42})
	}
	if run != nil {
		err = errors.Join(err, run.Close())
	}
	if err != nil {
		t.Fatal(err)
	}
	initial, cursor, err := ReadEventsAfter(root, snapshot.Meta.RunID, 0)
	if err != nil || len(initial) != 1 || cursor == 0 {
		t.Fatalf("первый batch не прочитан: %+v, cursor=%d, err=%v", initial, cursor, err)
	}

	next := RuntimeEvent{Time: time.Now().UTC(), RunID: snapshot.Meta.RunID, StepID: stepID, Kind: "step_state", State: "succeeded"}
	data, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	appendBytes := func(part []byte) {
		t.Helper()
		file, openErr := os.OpenFile(filepath.Join(root, snapshot.Meta.RunID, eventsFilename), os.O_APPEND|os.O_WRONLY, 0)
		if openErr == nil {
			_, openErr = file.Write(part)
		}
		if file != nil {
			openErr = errors.Join(openErr, file.Close())
		}
		if openErr != nil {
			t.Fatal(openErr)
		}
	}
	middle := len(data) / 2
	appendBytes(data[:middle])
	partial, unchanged, err := ReadEventsAfter(root, snapshot.Meta.RunID, cursor)
	if err != nil || len(partial) != 0 || unchanged != cursor {
		t.Fatalf("неполная строка принята: %+v, cursor=%d, err=%v", partial, unchanged, err)
	}
	remainder := append(append([]byte{}, data[middle:]...), '\n')
	appendBytes(remainder)
	completed, advanced, err := ReadEventsAfter(root, snapshot.Meta.RunID, cursor)
	if err != nil || len(completed) != 1 || completed[0].Kind != "step_state" || advanced <= cursor {
		t.Fatalf("дописанная строка не прочитана: %+v, cursor=%d, err=%v", completed, advanced, err)
	}
}

// TestSummarizeEventsTracksActiveItemTypes проверяет пользовательский смысл
// lifecycle: started добавляет действие, completed снимает только совпавший ID,
// а границы turn и процесса очищают незавершённые элементы после сбоя.
func TestSummarizeEventsTracksActiveItemTypes(t *testing.T) {
	base := []RuntimeEvent{
		{StepID: "step", Kind: "turn_started"},
		{StepID: "step", Kind: "item_started", ItemID: "command-1", ItemType: "commandExecution"},
		{StepID: "step", Kind: "item_started", ItemID: "command-2", ItemType: "commandExecution"},
		{StepID: "step", Kind: "item_started", ItemID: "mcp-1", ItemType: "mcpToolCall"},
		{StepID: "step", Kind: "item_completed", ItemID: "command-1", ItemType: "commandExecution"},
	}
	assertTypes := func(name string, events []RuntimeEvent, want ...string) {
		t.Helper()
		got := SummarizeEvents(events)["step"].ActiveItemTypes
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: текущие действия = %v, ожидались %v", name, got, want)
		}
	}

	assertTypes("два типа", base, "commandExecution", "mcpToolCall")
	assertTypes("последняя команда завершена", append(append([]RuntimeEvent{}, base...),
		RuntimeEvent{StepID: "step", Kind: "item_completed", ItemID: "command-2", ItemType: "commandExecution"}), "mcpToolCall")
	assertTypes("turn завершён", append(append([]RuntimeEvent{}, base...),
		RuntimeEvent{StepID: "step", Kind: "turn_completed"}))
	assertTypes("процесс перезапущен", append(append([]RuntimeEvent{}, base...),
		RuntimeEvent{StepID: "step", Kind: "process_started", PID: 42}))
	assertTypes("процесс завершён", append(append([]RuntimeEvent{}, base...),
		RuntimeEvent{StepID: "step", Kind: "process_exited"}))
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
