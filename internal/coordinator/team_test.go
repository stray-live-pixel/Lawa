package coordinator

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// Реально подготовленные команды обоих форматов читают одну цель и публикуют
// от назначенного автора. Повтор вызова не создаёт вторую реплику.
func TestTeamToolsInBothWorkflowFormats(t *testing.T) {
	for _, version := range []string{"legacy", "v2"} {
		t.Run(version, func(t *testing.T) {
			prefix, dependency := "", `"dependsOn":[]`
			if version == "v2" {
				prefix = `"version":2,"start":["work"],`
				dependency = `"after":[]`
			}
			root, initial, run := createAgentPreparationRun(t, `{`+prefix+`"id":"team","characters":{"boss":{"name":"Босс","history":"Начало","instructions":"Веди команду"}},"steps":[{"id":"work","type":"agent","character":"boss","prompt":"Работай",`+dependency+`}]}`)
			// Даже при наличии телеметрии в общем team.json подготовленный
			// инструмент обоих форматов возвращает только рабочую память.
			if err := runstore.UpdateTeam(t.Context(), root, initial.Meta.RunID, func(chat *runstore.TeamChat) error {
				chat.Metrics = &runstore.TeamMetrics{RecordedFrom: time.Now(), Executions: []*runstore.TeamExecution{{ID: "metric-sentinel", ActorID: "boss"}}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var command codex.Command
			if version == "legacy" {
				p, err := Prepare(run, root)
				if err != nil {
					t.Fatal(err)
				}
				command = p.Launches[0].Command
				defer p.Launches[0].lease.Release()
			} else {
				p, err := prepareAgentVisits(run, root, nil, false, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				command = p.Work[0].Command
				defer releaseAgentWork(p.Work)
			}
			if !strings.Contains(command.Text, "team_read") {
				t.Fatal("нет инструкции общей базы")
			}
			read, err := command.CallDynamicTool(t.Context(), codex.DynamicToolCall{Tool: "team_read", Arguments: []byte(`{}`)})
			if err != nil || !strings.Contains(read, initial.Meta.RunID) {
				t.Fatal(read, err)
			}
			if strings.Contains(read, `"metrics"`) || strings.Contains(read, "metric-sentinel") || strings.Contains(command.Text, "metric-sentinel") {
				t.Fatal("телеметрия попала в модельный контекст")
			}
			call := codex.DynamicToolCall{Tool: "team_post", ThreadID: "thread", TurnID: "turn", CallID: "call", Arguments: []byte(`{"text":"Проверки прошли"}`)}
			first, err := command.CallDynamicTool(t.Context(), call)
			if err != nil {
				t.Fatal(err)
			}
			again, err := command.CallDynamicTool(t.Context(), call)
			if err != nil || first != again {
				t.Fatal("retry", err)
			}
			var message runstore.TeamMessage
			if err = json.Unmarshal([]byte(first), &message); err != nil || message.AuthorID != initial.Meta.RunID+":character:boss" {
				t.Fatal(message, err)
			}
			call.Arguments = []byte(`{"text":"Факт","authorId":"human"}`)
			if _, err = command.CallDynamicTool(t.Context(), call); err == nil {
				t.Fatal("агент подменил автора")
			}
		})
	}
}
