package reviewruntime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// TestRuntimeAppServerProtocol проходит настоящий codex.Run и отдельный процесс
// для каждого этапа. Поддельный сервер не вызывает модель и не требует подписки.
func TestRuntimeAppServerProtocol(t *testing.T) {
	e, id := fixture(t)
	e.Executable = protocolExecutable(t)
	if err := e.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	r, err := e.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != reviewstore.Succeeded {
		t.Fatal(r.State)
	}
	for _, stage := range r.Stages {
		a := stage.Attempts[0]
		if a.ThreadID != "thread" || a.TurnID != "turn" || a.OutputPath == "" {
			t.Fatalf("протокол не сохранён: %+v", a)
		}
	}
}

// protocolExecutable создаёт только локальный test-сервер.
func protocolExecutable(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "codex")
	content := "#!/bin/sh\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run=^TestReviewRPCServer$ -- \"$@\"\n"
	if err = os.WriteFile(wrapper, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAWA_REVIEW_RPC_TEST", "1")
	return wrapper
}

// TestThreadUsageProtocol проверяет отдельное read-only чтение реального
// контрактного ответа billing API и защиту от перепутанного thread ID.
func TestThreadUsageProtocol(t *testing.T) {
	o, err := codex.OpenObserver(context.Background(), codex.Connection{Executable: protocolExecutable(t), CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	g, err := o.ReadUsage("child")
	if err != nil || len(g) != 1 || g[0].Model == nil || *g[0].InputTokens != 100 {
		t.Fatalf("%+v %v", g, err)
	}
	if _, err = o.ReadUsage("wrong"); err == nil {
		t.Fatal("принята разбивка другого thread")
	}
}

// TestReviewRPCServer обслуживает только test-процесс, создавая последовательность
// динамических вызовов с подтверждением каждого ответа перед следующим.
func TestReviewRPCServer(t *testing.T) {
	if os.Getenv("LAWA_REVIEW_RPC_TEST") != "1" {
		return
	}
	type call struct {
		Name string
		Args any
	}
	var calls []call
	index := 0
	enc := json.NewEncoder(os.Stdout)
	dec := json.NewDecoder(os.Stdin)
	send := func(v any) {
		if err := enc.Encode(v); err != nil {
			os.Exit(2)
		}
	}
	next := func() {
		if index < len(calls) {
			c := calls[index]
			index++
			send(map[string]any{"id": "tool-call", "method": "item/tool/call", "params": map[string]any{"threadId": "thread", "turnId": "turn", "callId": "call", "tool": c.Name, "arguments": c.Args}})
		} else {
			send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread", "item": map[string]any{"id": "message", "type": "agentMessage", "text": "Готово"}}})
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": "completed"}}})
		}
	}
	for {
		var m struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
			Result json.RawMessage
		}
		if err := dec.Decode(&m); err == io.EOF {
			os.Exit(0)
		} else if err != nil {
			os.Exit(2)
		}
		reply := func(result any) { send(map[string]any{"id": m.ID, "result": result}) }
		switch m.Method {
		case "account/usage/read":
			reply(map[string]any{"threadUsage": map[string]any{"threadId": "child", "groups": []any{map[string]any{"model": "gpt-6-sol", "reasoningEffort": "high", "inputTokens": 100, "cachedInputTokens": 40, "outputTokens": 10}}}})
		case "model/list":
			models := []any{}
			for _, name := range []string{"gpt-6-sol", "gpt-6-astra"} {
				models = append(models, map[string]any{"id": name, "model": name, "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}}})
			}
			reply(map[string]any{"data": models})
		case "initialize", "thread/name/set":
			reply(map[string]any{})
		case "initialized":
		case "thread/start":
			var p map[string]any
			json.Unmarshal(m.Params, &p)
			if p["permissions"] != "lawa_review" || p["cwd"] == "" || p["dynamicTools"] == nil {
				os.Exit(3)
			}
			reply(map[string]any{"thread": map[string]any{"id": "thread"}})
		case "turn/start":
			var p struct{ Input []struct{ Text string } }
			json.Unmarshal(m.Params, &p)
			text := p.Input[0].Text
			if strings.Contains(text, `этап "context"`) {
				calls = []call{{"review_artifact", artifactInput{Path: "artifacts/code.txt", Text: "login()\n"}}, {"review_context", contextInput{Title: "Вход", SourceRevision: "rev", Context: reviewstore.Context{ProjectFiles: []reviewstore.File{{ID: "file", Path: "login.go", SnapshotPath: "artifacts/code.txt", StartLine: 1, EndLine: 1, Change: "context"}}}}}}
			} else if strings.Contains(text, `этап "review"`) {
				calls = []call{{"review_findings", findingsInput{}}}
			} else {
				calls = []call{{"review_presentation", reviewstore.Presentation{Overview: reviewstore.Tour{ID: "overview", Title: "Вход", Steps: []reviewstore.TourStep{{ID: "s", Title: "Проверка входа", Body: "Отправляем данные.", Anchors: []reviewstore.Anchor{{FileID: "file", StartLine: 1, EndLine: 1}}}}}}}}
			}
			calls = append(calls, call{"review_finish", finishInput{}})
			reply(map[string]any{"turn": map[string]any{"id": "turn", "status": "inProgress"}})
			next()
		case "":
			var result struct{ Success bool }
			json.Unmarshal(m.Result, &result)
			if !result.Success {
				os.Stderr.Write(m.Result)
				os.Exit(4)
			}
			next()
		default:
			os.Exit(5)
		}
	}
}
