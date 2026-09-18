package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// viewURLWriter передаёт напечатанный URL без гонки с работающим сервером.
type viewURLWriter struct{ urls chan string }

func (w viewURLWriter) Write(p []byte) (int, error) {
	w.urls <- strings.TrimSpace(string(p))
	return len(p), nil
}

// TestViewLifecycle проходит через CLI с пустыми dependencies и пустым HOME:
// runtime не нужен, браузер может отказать, отмена освобождает порт и не пишет run.
func TestViewLifecycle(t *testing.T) {
	for _, noOpen := range []bool{true, false} {
		t.Run(map[bool]string{true: "no-open", false: "browser-failure"}[noOpen], func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			path := filepath.Join(root, "workflow.json")
			original := []byte(`{"id":"view","steps":[{"id":"step","type":"agent","prompt":"Hello","dependsOn":[]}]}`)
			if !noOpen {
				original = []byte(`{"version":2,"id":"view-v2","start":["step"],"steps":[{"id":"step","type":"agent","prompt":"Hello","after":[]}]}`)
			}
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			urls := make(chan string, 1)
			done := make(chan error, 1)
			var stderr bytes.Buffer
			go func() {
				if noOpen {
					done <- executeContext(ctx, []string{"view", path, "--no-open"}, viewURLWriter{urls}, &stderr, dependencies{})
				} else {
					done <- viewCommand(ctx, []string{path}, viewURLWriter{urls}, &stderr, func(context.Context, string) error { return errors.New("браузер недоступен") })
				}
			}()
			var url string
			select {
			case url = <-urls:
			case err := <-done:
				t.Fatalf("нет URL: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("нет URL")
			}
			client := &http.Client{Timeout: 10 * time.Second}
			response, err := client.Get(strings.TrimSuffix(url, "/view") + "/api/definition")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), `"Definition":true`) {
				t.Fatalf("просмотр недоступен: %s %v", body, err)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("сервер не остановлен")
			}
			if !noOpen && !strings.Contains(stderr.String(), "Откройте URL вручную") {
				t.Fatal("нет диагностики браузера")
			}
			address := strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/view")
			listener, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatalf("порт занят: %v", err)
			}
			listener.Close()
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, original) {
				t.Fatal("изменён исходник")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("созданы посторонние файлы: %v %v", entries, err)
			}
		})
	}
}

// TestViewInvalidSourceBeforeListen использует заведомо неверный listen: ошибка
// исходника должна появиться раньше попытки открыть порт и вызвать браузер.
func TestViewInvalidSourceBeforeListen(t *testing.T) {
	root := t.TempDir()
	for name, raw := range map[string]string{
		"json":     `{`,
		"graph":    `{"version":2,"id":"bad","start":["missing"],"steps":[]}`,
		"markdown": `{"id":"bad","steps":[{"id":"a","type":"agent","prompt":{"file":"missing.md"},"dependsOn":[]}]}`,
		"template": `{"id":"bad","steps":[{"id":"a","type":"agent","prompt":"{{missing}}","dependsOn":[]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name+".json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := viewCommand(t.Context(), []string{path, "--listen", "invalid"}, &out, io.Discard, func(context.Context, string) error { t.Fatal("браузер вызван"); return nil })
			if err == nil || !strings.Contains(err.Error(), "открыть workflow") || out.Len() != 0 {
				t.Fatalf("неверная ошибка: %v %s", err, out.String())
			}
		})
	}
	for _, args := range [][]string{nil, {"a", "b"}, {"a", "--listen="}, {"a", "--no-open", "--no-open"}, {"a", "--root", root}} {
		if err := viewCommand(t.Context(), args, io.Discard, io.Discard, nil); err == nil {
			t.Fatalf("приняты аргументы %v", args)
		}
	}
}
