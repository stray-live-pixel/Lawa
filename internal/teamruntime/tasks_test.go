package teamruntime

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// Публичные dynamic tools проходят полный цикл без VCS executable. Подменяется
// только App Server; JSON, runtime, lock, файлы и scheduler остаются настоящими.
func TestBoardPublicToolsWithoutVCS(t *testing.T) {
	e, run, client, _ := teamEngine(t)
	t.Setenv("PATH", t.TempDir())
	if err := runstore.UpdateTeam(t.Context(), e.Root, run, func(chat *runstore.TeamChat) error { chat.Room.TaskBoardVersion = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	execute := func(c codex.Command, author, action string, extra map[string]any) {
		chat := readChat(t, e, run)
		version := uint64(0)
		if task := chat.Room.Tasks["T1"]; task != nil {
			version = task.Card.Version
		}
		input := map[string]any{"id": author + "-" + action, "action": action, "taskId": "T1", "version": version}
		for k, v := range extra {
			input[k] = v
		}
		data, _ := json.Marshal(input)
		// Старый thread получает ровно ту же операцию через закреплённый team_post.
		text, _ := json.Marshal(map[string]string{"text": "/task " + string(data)})
		tool(t, c, "team_post", string(text))
	}
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "boss-board", "boss-create")
		tool(t, c, "team_summon", `{"id":"developer"}`)
		execute(c, "boss", "create", map[string]any{"title": "Файл без VCS", "body": "Создать result.txt", "expected": "Текстовый файл", "criteria": []string{"Содержит проверено"}, "assignee": "developer"})
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "boss")
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		if !strings.Contains(c.Text, "VCS может отсутствовать") {
			t.Fatal("нет нейтральной инструкции")
		}
		start(t, c, "dev-board", "dev-work")
		for _, action := range []string{"read", "acknowledge", "start"} {
			execute(c, "developer", action, nil)
		}
		if err := os.WriteFile(filepath.Join(c.CWD, "result.txt"), []byte("проверено"), 0600); err != nil {
			t.Fatal(err)
		}
		execute(c, "developer", "workspace", map[string]any{"workspace": map[string]string{"path": c.CWD, "method": "Обычная папка", "limitations": "Изоляция не заявлена; один тестовый файл"}})
		execute(c, "developer", "report", map[string]any{"result": map[string]any{"summary": "Готов файл", "artifacts": []string{"result.txt"}, "checks": []string{"Проверено содержимое"}, "limitations": "Синтетическая проверка без LLM"}})
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "developer")
	client.observed = codex.Observation{LatestTurnID: "boss-create"}
	// fake observer использует статус по Turns; отсутствие истории в этом тесте
	// позволяет новый boss thread, не меняя сохранённые данные задач.
	if err := runstore.UpdateTeam(t.Context(), e.Root, run, func(chat *runstore.TeamChat) error { chat.Room.Actors["boss"].ThreadID = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	client.execute = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		start(t, c, "boss-board-2", "boss-accept")
		task := readChat(t, e, run).Room.Tasks["T1"]
		result := task.Card.Results[0]
		execute(c, "boss", "review", map[string]any{"resultId": result.ID, "verdict": "approve", "reason": "Файл прочитан"})
		execute(c, "boss", "accept", map[string]any{"resultId": result.ID, "reason": "Соответствует критерию", "integration": "result.txt находится в папке результата"})
		tool(t, c, "team_task_read", `{"taskId":"T1","section":"messages","limit":1}`)
		return codex.Result{Status: "completed"}, nil
	}
	process(t, e, run, "boss")
	chat := readChat(t, e, run)
	task := chat.Room.Tasks["T1"]
	if task.Card.Status != "done" || task.AcceptedAt == nil || len(chat.Room.Tasks) != 1 {
		t.Fatalf("цикл не завершён: %+v", task)
	}
	var linked bool
	for _, m := range chat.Messages {
		if m.ID == "developer-report" {
			linked = m.TaskID == "T1" && m.ReplyTo == "boss-create"
		}
	}
	if !linked {
		t.Fatal("результат не связан с поручением")
	}
}
