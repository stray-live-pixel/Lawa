package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// bossClient принимает явные реплики вместо прежнего автоматического continue.
// Неопределённая доставка имитируется до OnTurn, чтобы проверить запрет дублей.
type bossClient struct {
	*cliFakeClient
	messages    []string
	ambiguous   bool
	unattempted bool
}

// Continue сохраняет серверную историю и отдаёт финальный отчёт тем же путём,
// что production: событие должно попасть в meta.json после terminal state.
func (c *bossClient) Continue(ctx context.Context, id string, command codex.Command) (codex.Result, error) {
	c.messages = append(c.messages, command.Text)
	if c.unattempted {
		return codex.Result{ThreadID: id}, errors.New("thread/resume отклонён до отправки")
	}
	if c.ambiguous {
		return codex.Result{ThreadID: id, TurnAttempted: true}, errors.New("потерян ответ turn/start")
	}
	turn := fmt.Sprintf("reply-%d", len(c.messages))
	if err := command.OnTurn(turn, func(context.Context) error { return nil }); err != nil {
		return codex.Result{ThreadID: id}, err
	}
	c.mu.Lock()
	c.latest[id] = turn
	c.mu.Unlock()
	if err := command.Notify(codex.Event{Method: "turn/started"}); err != nil {
		return codex.Result{ThreadID: id}, err
	}
	err := command.Notify(codex.Event{Method: "item/completed", Params: json.RawMessage(`{"item":{"type":"agentMessage","text":"Итог: Ответ Чела принят."}}`)})
	return codex.Result{ThreadID: id, TurnID: turn, TurnAttempted: true, Status: "completed"}, err
}

// TestOrderConversation доказывает сохранение исходной постановки, отдельной
// памяти личности и того же чата при ответе человека. Второй заказ независим.
func TestOrderConversation(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	client := &bossClient{cliFakeClient: newCLIFakeClient()}
	deps := cliTestDependencies(client, func(context.Context, codex.Connection) error { return nil })
	task := "  Хочу разобраться в большой задаче.\nНичего пока не меняй.  "
	client.onCommand = func(command codex.Command) error {
		if !strings.Contains(command.Text, task) || !strings.Contains(command.Text, "Личность: Босс") || len(command.Permissions.WritePaths) != 2 {
			return errors.New("Босс не получил исходный заказ, личность или свою память")
		}
		return command.Notify(codex.Event{Method: "item/completed", Params: json.RawMessage(`{"item":{"type":"agentMessage","text":"Итог: Какой результат нужен?"}}`)})
	}
	var out bytes.Buffer
	if err := executeContext(t.Context(), []string{"order", "--cwd", cwd, "--root", root, "--task", task}, &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	runID := strings.Fields(out.String())[1]
	first, err := runstore.Load(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Meta.Order == nil || !strings.Contains(first.Task, task) || !strings.Contains(out.String(), "Какой результат нужен?") {
		t.Fatal("утерян заказ или вопрос Босса")
	}
	if _, err = os.Stat(filepath.Join(root, runID, "memory", "character-boss.md")); err != nil {
		t.Fatal(err)
	}
	reply := "  Нужен отчёт\nбез изменений кода.  "
	// Достоверный отказ до сети не должен навсегда запирать диалог; следующий
	// явный reply разрешён. Неопределённая доставка проверяется отдельно ниже.
	client.unattempted = true
	if err = executeContext(t.Context(), []string{"reply", runID, "--root", root, "--task", reply}, io.Discard, io.Discard, deps); err == nil {
		t.Fatal("скрыт отказ до отправки сообщения")
	}
	client.unattempted = false
	client.messages = nil
	out.Reset()
	if err = executeContext(t.Context(), []string{"reply", runID, "--root", root, "--task", reply}, &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	after, err := runstore.Load(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Meta.Steps[0].CodexThreadID != first.Meta.Steps[0].CodexThreadID || after.Meta.Order.Pending || after.Meta.Order.Messages[0].Text != reply || after.Meta.Order.Messages[0].TurnID != "reply-1" || !strings.Contains(client.messages[0], reply) || !strings.Contains(out.String(), "Ответ Чела принят") {
		t.Fatal("reply потерял контекст или изменил реплику")
	}
	out.Reset()
	if err = executeContext(t.Context(), []string{"order", "--cwd", cwd, "--root", root, "--task", task}, &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if strings.Fields(out.String())[1] == runID {
		t.Fatal("новый заказ переиспользовал старого Босса")
	}
}

// TestOrderAmbiguousMessage не позволяет повторно отправить заказ после потери
// ответа транспорта. Появление нового turn в Codex позволяет связать историю.
func TestOrderAmbiguousMessage(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	client := &bossClient{cliFakeClient: newCLIFakeClient(), ambiguous: true}
	deps := cliTestDependencies(client, func(context.Context, codex.Connection) error { return nil })
	var out bytes.Buffer
	if err := executeContext(t.Context(), []string{"order", "--cwd", cwd, "--root", root, "--task", "Исследуй"}, &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	id := strings.Fields(out.String())[1]
	args := []string{"reply", id, "--root", root, "--task", "Уточнение"}
	if err := executeContext(t.Context(), args, io.Discard, io.Discard, deps); err == nil {
		t.Fatal("потеря ответа скрыта")
	}
	if err := executeContext(t.Context(), args, io.Discard, io.Discard, deps); err == nil || !strings.Contains(err.Error(), "неоднозначна") {
		t.Fatalf("повтор: %v", err)
	}
	if err := executeContext(t.Context(), []string{"resume", id, "--root", root}, io.Discard, io.Discard, deps); err == nil {
		t.Fatal("resume повторил неопределённое сообщение")
	}
	if len(client.messages) != 1 {
		t.Fatal("создан дублирующий turn")
	}
	client.mu.Lock()
	client.latest["chat-boss"] = "accepted-after-crash"
	client.mu.Unlock()
	if err := executeContext(t.Context(), []string{"resume", id, "--root", root}, io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runstore.Load(root, id)
	if err != nil || snapshot.Meta.Order.Pending || snapshot.Meta.Order.Messages[0].TurnID != "accepted-after-crash" {
		t.Fatalf("не восстановлена доставка: %v", err)
	}
}

// TestOrderAssignedWorkflow проходит делегирование через настоящий handler
// run_child. Удалённый исходник не влияет на снимок и профиль исполнителя.
func TestOrderAssignedWorkflow(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	source := filepath.Join(cwd, "workflow.json")
	data := `{"id":"build","characters":{"builder":{"name":"Мастер","history":"Исследовал платформеры","instructions":"Проверяй физику"}},"steps":[{"id":"build","type":"agent","character":"builder","prompt":"Собери прототип","dependsOn":[]}]}`
	if err := os.WriteFile(source, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	client := newCLIFakeClient()
	children := make(chan string, 1)
	client.onCommand = func(command codex.Command) error {
		if strings.Contains(command.Text, "characterId: builder") {
			children <- command.Text
			return nil
		}
		id := cliAgentPromptField(command.Text, "ID запуска (runId): ")
		if err := os.Remove(source); err != nil {
			return err
		}
		// Берём папку из выданных агенту прав, а не из аргумента --root:
		// последний может быть симлинком, например /var на macOS.
		args, _ := json.Marshal(map[string]string{"workflow": filepath.Join(command.Permissions.ReadPaths[0], runstore.AssignedWorkflowFilename), "cwd": cwd, "task": "Проверь физику", "parentRun": id})
		_, err := command.CallDynamicTool(t.Context(), codex.DynamicToolCall{Tool: "run_child", Arguments: args, ThreadID: "chat-boss", TurnID: "turn-boss", CallID: "child"})
		return err
	}
	deps := cliTestDependencies(client, func(context.Context, codex.Connection) error { return nil })
	if err := executeContext(t.Context(), []string{"order", source, "--cwd", cwd, "--root", root, "--task", "Сделай игру"}, io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	select {
	case <-children:
	default:
		t.Fatal("личность не выполнила workflow")
	}
}

// TestOrderDefinitionAndInput проверяет ошибки до создания чата, включая null,
// опечатку в личности, попытку выйти из memory и конфликт способов ввода.
func TestOrderDefinitionAndInput(t *testing.T) {
	for _, data := range []string{`null`, `{}`, `{"id":"x","characters":{"boss":{"name":"Босс","history":"История","instructions":"Действуй","typo":true}}}`, `{"id":"x","characters":{"../boss":{"name":"Босс","history":"История","instructions":"Действуй"}}}`} {
		path := filepath.Join(t.TempDir(), "person.json")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := orderDefinition(path); err == nil {
			t.Fatalf("принят неверный профиль: %s", data)
		}
	}
	path := filepath.Join(t.TempDir(), "boss.json")
	if err := os.WriteFile(path, []byte(`{"id":"room","characters":{"boss":{"name":"Босс Антон","history":"Опытный исследователь","instructions":"Не делай выводов без фактов"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	definition, assigned, err := orderDefinition(path)
	if err != nil || assigned != nil || definition.Characters["boss"].Name != "Босс Антон" {
		t.Fatalf("профиль без workflow: %v", err)
	}
	client := newCLIFakeClient()
	deps := cliTestDependencies(client, func(context.Context, codex.Connection) error {
		t.Error("неверный ввод дошёл до Codex")
		return nil
	})
	if err := executeContext(t.Context(), []string{"order", "--cwd", t.TempDir(), "--task", "x", "--task-file", path}, io.Discard, io.Discard, deps); err == nil {
		t.Fatal("приняты два способа ввода")
	}
}
