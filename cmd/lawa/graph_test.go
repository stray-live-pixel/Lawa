package main

import (
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/dashboard"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// TestGraphExport проверяет CLI и HTTP через настоящий pipe-процесс без Java.
// Подмена получает source в файл; это проверяет тему до внешнего рендера.
// Lock остаётся занятым: экспорт активного run не должен ждать координатор.
func TestGraphExport(t *testing.T) {
	root := t.TempDir()
	snapshot, err := runstore.Create(root, runstore.Input{CWD: root, Task: "Граф", WorkflowJSON: []byte(`{"version":2,"id":"graph","start":["cube"],"steps":[{"id":"cube","type":"agent","prompt":"Работа","after":[]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	sourcePath := filepath.Join(root, "source.txt")
	t.Setenv("LAWA_TEST_SOURCE", sourcePath)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	stub := "#!/bin/sh\ncat > \"$LAWA_TEST_SOURCE\"\nprintf '\\211PNG\\r\\n\\032\\n'\n"
	if err := os.WriteFile(filepath.Join(root, "plantuml"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(root, snapshot.Meta.RunID, "meta.json")
	before, _ := os.ReadFile(meta)
	for _, theme := range []string{"", "light"} {
		output := filepath.Join(root, "image-"+theme+".png")
		args := []string{"graph", snapshot.Meta.RunID, "--root", root, "--output", output}
		if theme != "" {
			args = append(args, "--theme", theme)
		}
		if err := executeContext(t.Context(), args, io.Discard, io.Discard, productionDependencies()); err != nil {
			t.Fatal(err)
		}
		if err := executeContext(t.Context(), args, io.Discard, io.Discard, productionDependencies()); err == nil {
			t.Fatal("перезаписан существующий файл")
		}
		recorder := httptest.NewRecorder()
		dashboard.Handler(root).ServeHTTP(recorder, httptest.NewRequest("GET", "/graph-image/"+snapshot.Meta.RunID+"?theme="+theme+"&download=1", nil))
		if recorder.Code != 200 || recorder.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(recorder.Header().Get("Content-Disposition"), "attachment;") {
			t.Fatalf("неверный экспорт: %d %s", recorder.Code, recorder.Body.String())
		}
		source, _ := os.ReadFile(sourcePath)
		background := "#080808"
		if theme == "light" {
			background = "#FFFFFF"
		}
		if !strings.Contains(string(source), "backgroundColor "+background) || !strings.Contains(string(source), "visit_0") {
			t.Fatalf("потеряны тема или посещение: %s", source)
		}
	}
	for _, query := range []string{"?theme=external", "?theme=dark"} {
		// Неверная тема отклоняется до renderer; ошибка процесса возвращается
		// как ошибка HTTP, а не как успешный ответ со старым PNG.
		if err := os.WriteFile(filepath.Join(root, "plantuml"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		dashboard.Handler(root).ServeHTTP(recorder, httptest.NewRequest("GET", "/graph-image/"+snapshot.Meta.RunID+query, nil))
		if recorder.Code < 400 || recorder.Header().Get("Content-Type") == "image/png" {
			t.Fatal("ошибка выдана за картинку")
		}
	}
	after, _ := os.ReadFile(meta)
	if !bytes.Equal(before, after) {
		t.Fatal("экспорт изменил состояние запуска")
	}
}
