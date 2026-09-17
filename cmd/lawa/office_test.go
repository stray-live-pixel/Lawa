package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// CLI — единственная точка создания и настройки. В тесте заменена только
// граница HTTP/Engine; снимок заказа сохраняется настоящим runstore.Create.
func TestServeCreatesConfiguredTeamAtStartup(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	config, err := filepath.Abs("../../examples/office/team.json")
	if err != nil {
		t.Fatal(err)
	}
	goalPath := filepath.Join(t.TempDir(), "goal.md")
	if err = os.WriteFile(goalPath, []byte("Создать удобный интерфейс\nс доступной палитрой"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	var run string
	deps := dependencies{serve: func(ctx context.Context, gotRoot, address string, startup func() error) error {
		entries, _ := os.ReadDir(root)
		if len(entries) != 0 || startup == nil || gotRoot != root {
			t.Fatal("создание произошло до готовности сервера")
		}
		if err := startup(); err != nil {
			return err
		}
		entries, _ = os.ReadDir(root)
		if len(entries) != 1 {
			t.Fatal("должен появиться ровно один заказ")
		}
		run = entries[0].Name()
		return nil
	}}
	err = serveCommand(t.Context(), []string{"--root", root, "--listen", "127.0.0.1:61000", "--team-config", config, "--cwd", cwd, "--task-file", goalPath}, &output, &output, deps)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := runstore.ReadTeam(root, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Room.Catalog) != 3 || len(chat.Room.Actors) != 1 || chat.Room.Catalog["designer"].Avatar != "pixel-designer" || chat.Goal != "Создать удобный интерфейс\nс доступной палитрой" {
		t.Fatal(chat)
	}
	if !strings.Contains(output.String(), "/office?run="+run) {
		t.Fatal(output.String())
	}
	// Повторный serve без цели только продолжает наблюдение существующего заказа.
	deps.serve = func(_ context.Context, _ string, _ string, startup func() error) error {
		if startup != nil {
			t.Fatal("serve без цели создаёт новый заказ")
		}
		return nil
	}
	if err = serveCommand(t.Context(), []string{"--root", root}, &output, &output, deps); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatal("заказ продублирован")
	}
}

// Флаги нельзя частично применить к уже работающей комнате или молча проигнорировать.
func TestServeTeamArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--team-config", "team.json"}, {"--cwd", "/tmp"}, {"--task", "Цель"},
		{"--cwd", "/tmp", "--task", "Цель", "--task-file", "goal.md"},
		{"--cwd", "/tmp", "--task", ""}, {"--cwd", "/tmp", "--task-file", ""},
		{"--cwd", "/tmp", "--task", "Цель", "--team-config", ""},
	} {
		if _, err := parseServeArguments(args); err == nil {
			t.Fatal("приняты некорректные флаги", args)
		}
	}
	parsed, err := parseServeArguments([]string{"--cwd", "/tmp", "--task", "Цель"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := officeInput(parsed)
	if err != nil {
		t.Fatal(err)
	}
	var definition workflow.Workflow
	if err = json.Unmarshal(input.WorkflowJSON, &definition); err != nil || len(definition.Characters) != 2 {
		t.Fatal(definition, err)
	}
}

// Неверный JSON отклоняется до открытия сервера. Ошибка самого сервера не
// создаёт заказ: запуск нельзя неожиданно подхватить при следующем serve.
func TestServeRejectsInvalidTeamBeforeStartup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	config := filepath.Join(t.TempDir(), "team.json")
	var output bytes.Buffer
	for _, body := range []string{
		`{}`, `null`, `{"characters":null}`, `{"characters":[]}`,
		`{"characters":{"human":{"name":"Чел","history":"Опыт","instructions":"Работай"}}}`,
		`{"characters":{"designer":{"name":"Дизайнер","instructions":"Работай"}}}`,
		`{"characters":{},"unexpected":true}`,
		`{"characters":{"designer":{"name":"Дизайнер","history":"Опыт","instructions":"Работай","role":"admin"}}}`,
	} {
		if err := os.WriteFile(config, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		deps := dependencies{serve: func(context.Context, string, string, func() error) error {
			t.Fatal("ошибка конфига дошла до сервера")
			return nil
		}}
		if err := serveCommand(t.Context(), []string{"--root", root, "--team-config", config, "--cwd", t.TempDir(), "--task", "Цель"}, &output, &output, deps); err == nil {
			t.Fatal(body)
		}
	}
	deps := dependencies{serve: func(context.Context, string, string, func() error) error { return errors.New("порт занят") }}
	if err := serveCommand(t.Context(), []string{"--root", root, "--cwd", t.TempDir(), "--task", "Цель"}, &output, &output, deps); err == nil {
		t.Fatal("потеряна ошибка сервера")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("создан незапрошенный заказ", err)
	}
}
