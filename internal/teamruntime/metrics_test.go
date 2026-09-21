package teamruntime

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/teamreport"
)

// Проверяется настоящий путь callback -> team.json -> team_read. Только известные
// счётчики своего turn переживают запись; модель видит рабочую память без метрик.
func TestMetricsRuntimeUsageAndAgentContext(t *testing.T) {
	e, run, client, now := teamEngine(t)
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		if err := c.OnThread("thread"); err != nil {
			t.Fatal(err)
		}
		if err := c.OnTurn("turn", nil); err != nil {
			t.Fatal(err)
		}
		initial := *now
		*now = now.Add(2 * time.Second)
		if err := c.OnTurn("turn", nil); err != nil {
			t.Fatal(err)
		}
		ex := readChat(t, e, run).Metrics.Executions[0]
		if !ex.StartedAt.Equal(initial) {
			t.Fatal("повтор сдвинул начало")
		}
		usage := func(thread, turn string, n int) codex.Event {
			data, _ := json.Marshal(map[string]any{"threadId": thread, "turnId": turn, "tokenUsage": map[string]any{"total": map[string]any{"inputTokens": n, "outputTokens": 3, "secret": "never store", "toolOutput": "never store", "extraNumber": 987654}, "last": map[string]any{"inputTokens": 2}}})
			return codex.Event{Method: "thread/tokenUsage/updated", Params: data}
		}
		for _, event := range []codex.Event{usage("thread", "turn", 10), usage("foreign", "turn", 999), usage("thread", "foreign", 999), usage("thread", "turn", -1)} {
			if err := c.Notify(event); err != nil {
				t.Fatal(err)
			}
		}
		if usage := readChat(t, e, run).Metrics.Executions[0].Usage; usage == nil || usage.Total == nil || *usage.Total.Input != 10 {
			t.Fatal("повреждён последний достоверный usage")
		}
		// Повтор того же снимка не накапливает токены.
		if err := c.Notify(usage("thread", "turn", 10)); err != nil {
			t.Fatal(err)
		}
		response, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{ThreadID: "thread", TurnID: "turn", CallID: "read", Tool: "team_read", Arguments: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{`"metrics"`, `"executions"`, `"claimedAt"`, `"usage"`, `"threadTotal"`} {
			if strings.Contains(response, field) {
				t.Fatalf("метрика в контексте %s", field)
			}
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(response), &object); err != nil {
			t.Fatal(err)
		}
		if _, ok := object["history"]; ok {
			t.Fatal("история в контексте")
		}
		if !strings.Contains(response, `"messages"`) || !strings.Contains(response, `"goal"`) {
			t.Fatal(response)
		}
		*now = now.Add(3 * time.Second)
		if err := c.Notify(codex.Event{Method: "turn/completed", Params: []byte(`{"threadId":"thread","turn":{"id":"turn","status":"completed","secret":"never store"}}`)}); err != nil {
			t.Fatal(err)
		}
		return codex.Result{Status: "completed", ThreadID: "thread", TurnID: "turn"}, nil
	}
	if err := e.Process(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	chat := readChat(t, e, run)
	ex := chat.Metrics.Executions[0]
	if len(chat.Metrics.Executions) != 1 || ex.FinishedAt.Sub(*ex.StartedAt) != 5*time.Second || ex.Usage == nil || *ex.Usage.Total.Input != 10 || ex.Usage.Total.CachedInput != nil {
		t.Fatalf("метрики: %+v", ex)
	}
	data, _ := json.Marshal(chat.Metrics)
	if strings.Contains(string(data), "never store") || strings.Contains(string(data), "987654") {
		t.Fatal(string(data))
	}
	if err := e.Process(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	if len(readChat(t, e, run).Metrics.Executions) != 1 {
		t.Fatal("пустой poll добавил исполнение")
	}
}

// После рестарта сохранённый terminal закрывает тот же интервал. Если terminal
// потерян, время наблюдения не считается временем работы до перезапуска.
func TestMetricsRecoveryNoDoubleCount(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "known"}[terminal], func(t *testing.T) {
			e, run, client, now := teamEngine(t)
			if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || !ok {
				t.Fatal(err)
			}
			if err := runstore.UpdateTeam(t.Context(), e.Root, run, func(chat *runstore.TeamChat) error {
				a := chat.Room.Actors["boss"]
				a.ThreadID = "thread"
				a.TurnID = "turn"
				a.Delivery.TurnID = "turn"
				a.Delivery.Attempted = true
				runstore.StartTeamExecution(chat, "boss", *now)
				if terminal {
					runstore.FinishTeamExecution(chat, "boss", "completed", now.Add(4*time.Second), false)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(time.Hour)
			client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "turn", LatestTurnStatus: "completed"}
			if err := e.Process(t.Context(), run, "boss"); err != nil {
				t.Fatal(err)
			}
			if err := e.Process(t.Context(), run, "boss"); err != nil {
				t.Fatal(err)
			}
			chat := readChat(t, e, run)
			report := teamreport.Build(chat, time.Time{})
			if len(chat.Metrics.Executions) != 1 || client.calls != 0 {
				t.Fatal("повтор исполнения")
			}
			if terminal && report.KnownActiveAgentSeconds != 4 {
				t.Fatal(report)
			}
			if !terminal && (report.ActiveAgentSeconds != nil || report.KnownActiveAgentSeconds != 0 || chat.Metrics.Executions[0].FinishedAt != nil) {
				t.Fatal(report)
			}
		})
	}
}

// Чтение старого team.json не добавляет поля и не мигрирует файл на диске.
func TestMetricsLegacyReadOnly(t *testing.T) {
	e, run, _, _ := teamEngine(t)
	chat := readChat(t, e, run)
	chat.Metrics = nil
	data, _ := json.Marshal(chat)
	path := filepath.Join(e.Root, run, "team.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got := readChat(t, e, run)
	report := teamreport.Build(got, time.Time{})
	if report.ActiveAgentSeconds != nil || got.Metrics != nil {
		t.Fatal(report)
	}
	after, _ := os.ReadFile(path)
	if string(data) != string(after) {
		t.Fatal("чтение изменило файл")
	}
}
