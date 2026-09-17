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
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// Новый ID проходит весь путь: каталог → tool → приглашение → личный prompt →
// ответ. Проверяем ограничения сервера независимо от того, послушалась ли модель.
func TestConfiguredDesignerLifecycle(t *testing.T) {
	root := t.TempDir()
	characters := workflow.DefaultTeamCharacters()
	characters["designer"] = workflow.Character{Name: "Дизайнер", History: "Исследователь интерфейсов", Instructions: "Проверяй контраст и удобство", Avatar: "pixel-designer"}
	definition := workflow.Workflow{ID: "office", Characters: characters, Steps: []workflow.Step{{ID: "boss", Type: "agent", Character: "boss", Prompt: "Работай", DependsOn: []string{}}}}
	data, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	s, err := runstore.Create(root, runstore.Input{Order: true, Team: true, CWD: t.TempDir(), Task: "Спроектировать интерфейс", WorkflowJSON: data})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	client := &fakeClient{}
	e := &Engine{Root: root, Client: client, Now: func() time.Time { return now }}
	run := s.Meta.RunID
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		if !strings.Contains(c.Text, "Исследователь интерфейсов") {
			t.Fatal("Босс не видит каталог")
		}
		found := false
		for _, tool := range c.DynamicTools {
			if tool.Name == "team_summon" && strings.Contains(string(tool.InputSchema), `"designer"`) {
				found = true
			}
		}
		if !found {
			t.Fatal("schema не содержит дизайнера")
		}
		start(t, c, "boss-thread", "boss-turn")
		for _, id := range []string{"stranger", "human", "boss", "../designer"} {
			args, _ := json.Marshal(map[string]string{"id": id})
			if _, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{Tool: "team_summon", Arguments: args}); err == nil {
				t.Fatalf("приглашён %s", id)
			}
		}
		tool(t, c, "team_summon", `{"id":"designer"}`)
		tool(t, c, "team_summon", `{"id":"designer"}`)
		tool(t, c, "team_post", `{"text":"@designer Проверь интерфейс"}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "boss")
	chat := readChat(t, e, run)
	if len(chat.Room.Actors) != 2 || chat.Members["designer"].Avatar != "pixel-designer" || chat.Room.Actors["boss"].Status != "monitoring" {
		t.Fatal(chat.Room, chat.Members)
	}
	if len(chat.History.Frames[0].Actors) != 1 {
		t.Fatal("личность появилась раньше приглашения")
	}
	process(t, e, run, "designer")
	if client.calls != 1 {
		t.Fatal("таймер обойдён")
	}
	// Прямой вопрос Чела будит и Босса даже до первого достижения цели.
	postHuman(t, e, run, "question", "@designer Какой цвет подходит?")
	chat = readChat(t, e, run)
	if !chat.Messages[len(chat.Messages)-1].NotifyBoss {
		t.Fatal("Босс не уведомлён")
	}
	if _, err := runstore.ClaimTeamDelivery(t.Context(), root, run, "boss", now); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.PostActor(t.Context(), root, run, "boss", "premature", "@human Дизайнер проверяет цвет. Есть пожелания?"); err != nil {
		t.Fatal("Босс не может уточнить пожелания", err)
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		if !strings.Contains(c.Text, "Проверяй контраст и удобство") || !strings.Contains(c.Text, "Ты Дизайнер (@designer)") {
			t.Fatal("утрачена личность")
		}
		start(t, c, "designer-thread", "designer-turn")
		for _, text := range []string{"@human Синий", "@developer Помоги"} {
			args, _ := json.Marshal(map[string]string{"text": text})
			if _, err := c.CallDynamicTool(ctx, codex.DynamicToolCall{Tool: "team_post", CallID: text, Arguments: args}); err == nil {
				t.Fatal("нарушены границы ответа", text)
			}
		}
		tool(t, c, "team_post", `{"text":"@boss Синий подходит, контраст проверен"}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "designer")
	if client.calls != 2 {
		t.Fatal("Чел не разбудил дизайнера")
	}
	if _, err := runstore.ClaimTeamDelivery(t.Context(), root, run, "boss", now); err != nil {
		t.Fatal(err)
	}
	// Полученный отчёт ещё не закрывает поручения: Босс отдельно проверяет их.
	chat = readChat(t, e, run)
	var taskIDs []string
	for id := range chat.Room.Tasks {
		taskIDs = append(taskIDs, id)
	}
	resultID := chat.Messages[len(chat.Messages)-1].ID
	if _, err := runstore.AcceptTeamTasks(t.Context(), root, run, "boss", "accepted", taskIDs, resultID, "Проверил контраст и соответствие сценарию"); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.CompleteTeam(t.Context(), root, run, "boss", "complete", "@human Дизайнер проверил синий цвет."); err != nil {
		t.Fatal(err)
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	postHuman(t, e, run, "followup", "@designer А зелёный?")
	chat = readChat(t, e, run)
	if chat.Room.AchievedAt != nil || chat.Room.Actors["boss"].NextCheck.IsZero() || chat.Room.Actors["designer"].NextCheck.IsZero() {
		t.Fatal("не возобновлена команда")
	}
	// После перезапуска новый Engine читает ту же личность и продолжает её thread.
	client.observed = codex.Observation{ThreadStatus: "idle", LatestTurnID: "designer-turn", LatestTurnStatus: "completed"}
	next := &Engine{Root: root, Client: client, Now: func() time.Time { return now }}
	process(t, next, run, "designer")
	if len(client.continued) != 1 || client.continued[0] != "designer-thread" {
		t.Fatal("потеряна память", client.continued)
	}
}

// Legacy-каталог восстанавливается без запуска модели и без изменения файла.
// Старые thread продолжают видеть ровно прежние роли и прежние персональные locks.
func TestLegacyRoomCatalog(t *testing.T) {
	e, run, _, _ := teamEngine(t)
	chat := readChat(t, e, run)
	chat.Room.Catalog = nil
	data, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Root, run, "team.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	restored := readChat(t, e, run)
	if len(restored.Room.Catalog) != 2 || restored.Room.Catalog["developer"].Name != "Разработчик" {
		t.Fatal(restored.Room)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("GET изменил legacy-файл")
	}
}
