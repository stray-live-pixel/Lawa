package coordinator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// TestCodexEventObservabilityBoundary фиксирует два интерфейса одного журнала:
// Dashboard получает выбранный live-контент, а raw reasoning, аргументы и result
// инструментов по-прежнему не проходят границу расширяемого протокола.
func TestCodexEventObservabilityBoundary(t *testing.T) {
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
		event("item/reasoning/summaryTextDelta", `{"itemId":"reasoning-1","delta":"Проверяю подход"}`),
		event("future/unknown", `{not valid json`),
		event("item/completed", `{"item":{"id":"reasoning-1","type":"reasoning","text":"private reasoning summary"}}`),
		event("item/started", `{"item":{"id":"command-1","type":"commandExecution","command":"go test ./..."}}`),
		event("item/commandExecution/outputDelta", `{"itemId":"command-1","delta":"ok\n"}`),
		event("item/completed", `{"item":{"id":"command-1","type":"commandExecution","command":"go test ./...","aggregatedOutput":"private final copy"}}`),
		event("item/started", `{"item":{"id":"mcp-1","type":"mcpToolCall","server":"github","tool":"get_issue","arguments":{"token":"secret argument"}}}`),
		event("item/agentMessage/delta", `{"itemId":"message-1","delta":"готов"}`),
		event("item/agentMessage/delta", `{"itemId":"message-1","delta":"о"}`),
		event("item/completed", `{"item":{"id":"message-1","type":"agentMessage","text":"готово\nпубличный итог"}}`),
		event("thread/tokenUsage/updated", `{"tokenUsage":{"inputTokens":12,"outputTokens":3,"secret":"never store"}}`),
		event("thread/status/changed", `{"thread":{"status":{"type":"active"}}}`),
	} {
		if err = appendCodexEvent(run, stepID, "thread-1", "turn-1", input); err != nil {
			t.Fatal(err)
		}
	}
	events, err := runstore.ReadEvents(root, runID)
	if err != nil || len(events) != 10 {
		t.Fatalf("неверный нормализованный журнал: %+v, %v", events, err)
	}
	if events[0].Kind != "reasoning_summary_delta" || events[0].Content != "Проверяю подход" ||
		events[1].Kind != "item_started" || events[1].Content != "go test ./..." ||
		events[2].Kind != "command_output_delta" || events[2].Content != "ok\n" ||
		events[3].Kind != "item_completed" || events[3].Content != "" ||
		events[4].Content != "github · get_issue" || events[5].Content+events[6].Content != "готово" ||
		events[7].Message != "готово публичный итог" || events[7].Content != "готово\nпубличный итог" ||
		events[8].Usage["tokenUsage.inputTokens"] != 12 || events[8].Usage["tokenUsage.outputTokens"] != 3 || events[9].State != "active" {
		t.Fatalf("граница observability нарушена: %+v", events)
	}
	stored, err := json.Marshal(events)
	if err != nil || strings.Contains(string(stored), "private reasoning") || strings.Contains(string(stored), "private final copy") || strings.Contains(string(stored), "secret argument") {
		t.Fatalf("журнал сохранил запрещённый payload: %s, %v", stored, err)
	}
	if formatted := runstore.FormatEvent(events[2]); strings.Contains(formatted, "ok") {
		t.Fatalf("безопасный logs раскрыл Content: %q", formatted)
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
