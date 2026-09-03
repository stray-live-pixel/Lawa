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

// TestAgentGraphRuntimeEventReferences проверяет durable-адрес события v4.
// Ранний process_started появляется до ID чата, а последующие события
// могут указать только thread/turn, уже атомарно связанные с этим visit.
func TestAgentGraphRuntimeEventReferences(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Create(root, agentGraphInput(t))
	if err != nil {
		t.Fatal(err)
	}
	visit := snapshot.Meta.Visits[0]
	run, err := OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := run.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()

	started := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	if err = run.ReserveVisits([]string{visit.VisitID}); err == nil {
		err = run.AppendEvent(RuntimeEvent{
			Time: started, VisitID: visit.VisitID, StepID: visit.StepID,
			Kind: "process_started", PID: 4242,
		})
	}
	if err == nil {
		err = run.UpdateVisit(visit.VisitID, scheduler.Unknown, "chat-v4", "")
	}
	if err == nil {
		err = run.AppendEvent(RuntimeEvent{
			Time: started.Add(time.Second), VisitID: visit.VisitID, StepID: visit.StepID,
			ThreadID: "chat-v4", Kind: "thread_started",
		})
	}
	if err == nil {
		err = run.SetVisitTurn(visit.VisitID, "turn-v4")
	}
	if err == nil {
		err = run.AppendEvent(RuntimeEvent{
			Time: started.Add(2 * time.Second), VisitID: visit.VisitID, StepID: visit.StepID,
			ThreadID: "chat-v4", TurnID: "turn-v4", Kind: "turn_bound",
		})
	}
	if err != nil {
		t.Fatal(err)
	}

	invalid := map[string]RuntimeEvent{
		"без visit": {
			StepID: visit.StepID, Kind: "error",
		},
		"неизвестный visit": {
			VisitID: newID(), StepID: visit.StepID, Kind: "error",
		},
		"без step": {
			VisitID: visit.VisitID, Kind: "error",
		},
		"чужой step": {
			VisitID: visit.VisitID, StepID: snapshot.Meta.Visits[1].StepID, Kind: "error",
		},
		"чужой thread": {
			VisitID: visit.VisitID, StepID: visit.StepID, ThreadID: "other-chat", Kind: "error",
		},
		"turn без thread": {
			VisitID: visit.VisitID, StepID: visit.StepID, TurnID: "turn-v4", Kind: "error",
		},
		"чужой turn": {
			VisitID: visit.VisitID, StepID: visit.StepID, ThreadID: "chat-v4", TurnID: "other-turn", Kind: "error",
		},
	}
	for name, event := range invalid {
		t.Run(name, func(t *testing.T) {
			if appendErr := run.AppendEvent(event); appendErr == nil {
				t.Fatalf("несогласованное событие принято: %+v", event)
			}
		})
	}
	// Visit хранит только последний TurnID, но журнал обязан оставаться читаемым
	// после продолжения того же чата. На append новый ID сверяется с metadata;
	// reader затем принимает и первый, и второй turn как историю одного visit.
	if err = run.SetVisitTurn(visit.VisitID, "turn-v4-next"); err == nil {
		err = run.AppendEvent(RuntimeEvent{
			Time: started.Add(3 * time.Second), VisitID: visit.VisitID, StepID: visit.StepID,
			ThreadID: "chat-v4", TurnID: "turn-v4-next", Kind: "turn_bound",
		})
	}
	if err != nil {
		t.Fatal(err)
	}

	events, err := ReadEvents(root, snapshot.Meta.RunID)
	if err != nil || len(events) != 4 || events[2].TurnID != "turn-v4" || events[3].TurnID != "turn-v4-next" {
		t.Fatalf("валидные v4 события не прочитаны: %+v, %v", events, err)
	}
	for _, event := range events {
		if event.VisitID != visit.VisitID || event.StepID != visit.StepID {
			t.Fatalf("visit/step не прошли JSONL roundtrip: %+v", event)
		}
	}
	formatted := FormatEvent(events[3])
	if !strings.Contains(formatted, "visit="+visit.VisitID) {
		t.Fatalf("FormatEvent не показал visit: %q", formatted)
	}
}

// TestReadEventsRejectsForeignAgentGraphScope проверяет read-only границу:
// ручная порча JSONL не должна подменить visit в status или dashboard.
func TestReadEventsRejectsForeignAgentGraphScope(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Create(root, agentGraphInput(t))
	if err != nil {
		t.Fatal(err)
	}
	event := RuntimeEvent{
		Time: time.Now().UTC(), RunID: snapshot.Meta.RunID, VisitID: newID(),
		StepID: snapshot.Meta.Visits[0].StepID, Kind: "error",
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err = os.WriteFile(filepath.Join(root, snapshot.Meta.RunID, eventsFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if events, readErr := ReadEvents(root, snapshot.Meta.RunID); readErr == nil || events != nil {
		t.Fatalf("читатель принял чужой visit: %+v, %v", events, readErr)
	}
	if events, cursor, readErr := ReadEventsAfter(root, snapshot.Meta.RunID, 0); readErr == nil || events != nil || cursor != 0 {
		t.Fatalf("incremental reader принял чужой visit: %+v, cursor=%d, %v", events, cursor, readErr)
	}
}

// TestEventReadersReloadVisitsAfterBatch фиксирует гонку между двумя отдельными
// файлами append-only run. Writer сначала атомарно публикует новый visit в meta,
// затем его первое событие; reader, взявший прежний snapshot до этого окна,
// обязан перечитать metadata после batch и не объявлять честное событие чужим.
func TestEventReadersReloadVisitsAfterBatch(t *testing.T) {
	tests := []struct {
		name string
		read func(string, string, func() error) ([]RuntimeEvent, error)
	}{
		{
			name: "full",
			read: func(root, runID string, hook func() error) ([]RuntimeEvent, error) {
				return readEvents(root, runID, hook)
			},
		},
		{
			name: "after offset",
			read: func(root, runID string, hook func() error) ([]RuntimeEvent, error) {
				events, _, err := readEventsAfter(root, runID, 0, hook)
				return events, err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, initial, run := testAdvanceRun(t)
			finishDecisionVisit(t, run, initial.Meta.Visits[0].VisitID, "branch")
			var created Visit
			events, err := tc.read(root, initial.Meta.RunID, func() error {
				advanced, advanceErr := run.AdvanceAgentGraph()
				if advanceErr != nil {
					return advanceErr
				}
				created = advanced.CreatedVisits[0]
				return run.AppendEvent(RuntimeEvent{VisitID: created.VisitID, StepID: created.StepID, Kind: "process_started", PID: 42})
			})
			if err != nil || len(events) != 1 || events[0].VisitID != created.VisitID {
				t.Fatalf("reader не увидел visit из более свежей metadata: %+v, %v", events, err)
			}
		})
	}
}

// TestAgentGraphSummariesSeparateVisits защищает dashboard цикла: два прохода
// одного StepID хранят свои PID и item lifecycle в разных сводках.
func TestAgentGraphSummariesSeparateVisits(t *testing.T) {
	first, second := newID(), newID()
	events := []RuntimeEvent{
		{StepID: "loop", VisitID: first, Kind: "process_started", PID: 101},
		{StepID: "loop", VisitID: first, Kind: "item_started", ItemID: "shared-item", ItemType: "commandExecution"},
		{StepID: "loop", VisitID: second, Kind: "process_started", PID: 202},
		{StepID: "loop", VisitID: second, Kind: "item_started", ItemID: "shared-item", ItemType: "mcpToolCall"},
		{StepID: "loop", VisitID: second, Kind: "item_completed", ItemID: "shared-item", ItemType: "mcpToolCall"},
	}
	summaries := SummarizeEvents(events)
	if len(summaries) != 2 {
		t.Fatalf("посещения одного шага смешаны: %+v", summaries)
	}
	firstSummary, secondSummary := summaries[first], summaries[second]
	if firstSummary.VisitID != first || firstSummary.StepID != "loop" || firstSummary.PID != 101 ||
		strings.Join(firstSummary.ActiveItemTypes, ",") != "commandExecution" {
		t.Fatalf("первое посещение потеряло своё состояние: %+v", firstSummary)
	}
	if secondSummary.VisitID != second || secondSummary.StepID != "loop" || secondSummary.PID != 202 ||
		len(secondSummary.ActiveItemTypes) != 0 {
		t.Fatalf("второе посещение потеряло своё состояние: %+v", secondSummary)
	}
}

// TestLegacyRuntimeEventRejectsVisitID фиксирует границу форматов:
// старые run по-прежнему принимают step и run-level события, но не смешивают
// их с новой visit-семантикой. Старая JSONL-строка также остаётся читаемой.
func TestLegacyRuntimeEventRejectsVisitID(t *testing.T) {
	root := t.TempDir()
	snapshot, err := Create(root, testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	stepID := snapshot.Meta.Steps[0].ID
	if err = run.AppendEvent(RuntimeEvent{StepID: stepID, VisitID: newID(), Kind: "error"}); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy-run принял visitId: %v", err)
	}
	if err = run.AppendEvent(RuntimeEvent{StepID: stepID, Kind: "step_state", State: "pending"}); err == nil {
		err = run.AppendEvent(RuntimeEvent{Kind: "token_usage_updated", Usage: map[string]int64{"total": 1}})
	}
	if closeErr := run.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	events, err := ReadEvents(root, snapshot.Meta.RunID)
	if err != nil || len(events) != 2 || events[0].VisitID != "" || events[1].StepID != "" {
		t.Fatalf("legacy-контракт изменился: %+v, %v", events, err)
	}
	old, err := parseRuntimeEvent([]byte(`{"time":"2026-09-02T12:00:00Z","runId":"legacy-run","stepId":"step","kind":"step_state"}`), "legacy-run")
	if err != nil || old.VisitID != "" || old.StepID != "step" {
		t.Fatalf("старая JSONL-строка не читается: %+v, %v", old, err)
	}
	legacySummary := SummarizeEvents(events)
	if len(legacySummary) != 1 || legacySummary[stepID].StepID != stepID {
		t.Fatalf("legacy-сводка больше не ключевана по stepId: %+v", legacySummary)
	}
}

// TestFormatEventEscapesTerminalControls защищает CLI от ANSI/OSC injection:
// данные остаются различимыми, но ни один управляющий байт не достигает терминала.
func TestFormatEventEscapesTerminalControls(t *testing.T) {
	event := RuntimeEvent{
		Time: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), Kind: "error\x1b[2J",
		StepID: "step\x07", ThreadID: "thread\u009b", Message: "первая\nвторая\u2028третья\u2029четвёртая",
		Usage: map[string]int64{"tokens\x1b]52;c;secret\x07": 1},
	}
	formatted := FormatEvent(event)
	for _, forbidden := range []string{"\x1b", "\x07", "\u009b", "\n", "\u2028", "\u2029"} {
		if strings.Contains(formatted, forbidden) {
			t.Fatalf("FormatEvent оставил управляющий символ %q: %q", forbidden, formatted)
		}
	}
	for _, visible := range []string{`\u001B[2J`, `step\u0007`, `thread\u009B`, `первая\u000Aвторая\u2028третья\u2029четвёртая`, `\u001B]52;c;secret\u0007`} {
		if !strings.Contains(formatted, visible) {
			t.Fatalf("FormatEvent потерял видимое экранирование %q: %q", visible, formatted)
		}
	}
}

// TestTraceContentPreservesBrowserLayout проверяет новый приватный слой:
// переносы stdout остаются читаемыми в боковой панели, terminal controls
// удаляются, а обычный текстовый журнал по-прежнему Content не раскрывает.
func TestTraceContentPreservesBrowserLayout(t *testing.T) {
	event := RuntimeEvent{
		Time: time.Now(), RunID: "run", Kind: "command_output_delta",
		Content: "первая\r\n\tвторая\x1b[2J",
	}
	if err := normalizeRuntimeEvent(&event); err != nil {
		t.Fatal(err)
	}
	if event.Content != "первая\n\tвторая[2J" {
		t.Fatalf("layout live-вывода искажён: %q", event.Content)
	}
	if strings.Contains(FormatEvent(event), "первая") {
		t.Fatalf("FormatEvent раскрыл приватный Content: %q", FormatEvent(event))
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
