package coordinator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// TestCodexEventPrivacyBoundary фиксирует allow-list observability: полезный
// lifecycle сохраняется, а reasoning и содержимое command/tool payload — нет.
func TestCodexEventPrivacyBoundary(t *testing.T) {
	root, run := createExecutionRun(t, `{"id":"one","steps":[{"id":"step","type":"agent","prompt":"work","dependsOn":[]}]}`)
	snapshot, err := run.Load()
	if err != nil {
		t.Fatal(err)
	}
	stepID, runID := snapshot.Meta.Steps[0].ID, snapshot.Meta.RunID
	event := func(method, params string) codex.Event {
		return codex.Event{Method: method, Params: json.RawMessage(params)}
	}
	for _, input := range []codex.Event{
		event("item/reasoning/delta", `{"delta":"private reasoning"}`),
		event("future/unknown", `{not valid json`),
		event("item/completed", `{"item":{"id":"reasoning-1","type":"reasoning","text":"private reasoning summary"}}`),
		event("item/started", `{"item":{"id":"command-1","type":"commandExecution","command":"secret --token abc"}}`),
		event("item/completed", `{"item":{"id":"command-1","type":"commandExecution","command":"secret --token abc","aggregatedOutput":"private output"}}`),
		event("item/completed", `{"item":{"id":"message-1","type":"agentMessage","text":"готово\nпубличный итог"}}`),
		event("thread/tokenUsage/updated", `{"tokenUsage":{"inputTokens":12,"outputTokens":3,"secret":"never store"}}`),
		event("thread/status/changed", `{"thread":{"status":{"type":"active"}}}`),
	} {
		if err = appendCodexEvent(run, stepID, "thread-1", "turn-1", input); err != nil {
			t.Fatal(err)
		}
	}
	events, err := runstore.ReadEvents(root, runID)
	if err != nil || len(events) != 5 {
		t.Fatalf("неверный нормализованный журнал: %+v, %v", events, err)
	}
	if events[0].Kind != "item_started" || events[0].ItemID != "command-1" || events[0].ItemType != "commandExecution" || events[0].Message != "" ||
		events[1].Kind != "item_completed" || events[1].ItemID != "command-1" || events[1].Message != "" ||
		events[2].ItemID != "message-1" || events[2].ItemType != "agentMessage" || events[2].Message != "готово публичный итог" ||
		events[3].Usage["tokenUsage.inputTokens"] != 12 || events[3].Usage["tokenUsage.outputTokens"] != 3 ||
		events[4].State != "active" {
		t.Fatalf("граница приватности нарушена: %+v", events)
	}
	stored, err := json.Marshal(events)
	if err != nil || strings.Contains(string(stored), "secret") || strings.Contains(string(stored), "private output") {
		t.Fatalf("журнал сохранил command payload: %s, %v", stored, err)
	}
}

// TestNumericLeavesDeterministic проверяет, что лимит usage не зависит от
// случайного порядка map и при каждом запуске выбирает одинаковые поля.
func TestNumericLeavesDeterministic(t *testing.T) {
	root := map[string]any{
		"z": json.Number("3"),
		"a": map[string]any{"second": json.Number("2"), "first": json.Number("1")},
	}
	want := map[string]int64{"a.first": 1, "a.second": 2}
	for attempt := 0; attempt < 20; attempt++ {
		got := numericLeaves(root, 2)
		if len(got) != len(want) || got["a.first"] != want["a.first"] || got["a.second"] != want["a.second"] {
			t.Fatalf("лимит usage применён недетерминированно: %+v", got)
		}
	}
}
