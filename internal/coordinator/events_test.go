package coordinator

import (
	"encoding/json"
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
		event("item/completed", `{"item":{"type":"reasoning","text":"private reasoning summary"}}`),
		event("item/completed", `{"item":{"type":"commandExecution","command":"secret --token abc","aggregatedOutput":"private output"}}`),
		event("item/completed", `{"item":{"type":"agentMessage","text":"готово\nпубличный итог"}}`),
		event("thread/tokenUsage/updated", `{"tokenUsage":{"inputTokens":12,"outputTokens":3,"secret":"never store"}}`),
		event("thread/status/changed", `{"thread":{"status":{"type":"active"}}}`),
	} {
		if err = appendCodexEvent(run, stepID, "thread-1", "turn-1", input); err != nil {
			t.Fatal(err)
		}
	}
	events, err := runstore.ReadEvents(root, runID)
	if err != nil || len(events) != 4 {
		t.Fatalf("неверный нормализованный журнал: %+v, %v", events, err)
	}
	if events[0].ItemType != "commandExecution" || events[0].Message != "" ||
		events[1].ItemType != "agentMessage" || events[1].Message != "готово публичный итог" ||
		events[2].Usage["tokenUsage.inputTokens"] != 12 || events[2].Usage["tokenUsage.outputTokens"] != 3 ||
		events[3].State != "active" {
		t.Fatalf("граница приватности нарушена: %+v", events)
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
