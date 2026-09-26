package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// TestReviewCommandUIDefault проверяет открытие именно reviewId и явный no-ui.
func TestReviewCommandUIDefault(t *testing.T) {
	for _, noUI := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "no-ui"}[noUI], func(t *testing.T) {
			root, cwd := t.TempDir(), t.TempDir()
			var started, opened, ran bool
			var id string
			deps := dependencies{
				startUI: func(context.Context, string) (*runUIServer, error) {
					started = true
					return &runUIServer{url: "http://localhost:1234", shared: true}, nil
				},
				openBrowser: func(_ context.Context, url string) error {
					opened = true
					if url != "http://localhost:1234/code-review?reviewId="+id {
						t.Fatalf("URL: %s", url)
					}
					return nil
				},
				reviewRun: func(_ context.Context, gotRoot, gotID, executable string) error {
					ran = true
					if gotRoot != root || gotID != id {
						t.Fatal("не тот snapshot")
					}
					return nil
				},
			}
			// ID становится доступен после печати заголовка и до открытия браузера.
			out := &captureReviewID{onID: func(s string) { id = s }}
			args := []string{"--prompt", "Проведи ревью", "--root", root, "--cwd", cwd}
			if noUI {
				args = append(args, "--no-ui")
			}
			if err := reviewCommand(context.Background(), args, out, io.Discard, deps); err != nil {
				t.Fatal(err)
			}
			if !ran || started == noUI || opened == noUI {
				t.Fatalf("ran=%v started=%v opened=%v", ran, started, opened)
			}
			v, err := reviewstore.New(root).Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if v.Config.Review.Model != "gpt-6-astra" {
				t.Fatal(v.Config)
			}
		})
	}
}

// captureReviewID наблюдает обычный CLI-вывод, не вмешиваясь в создание сущности.
type captureReviewID struct {
	bytes.Buffer
	onID func(string)
}

func (w *captureReviewID) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	line := strings.TrimSpace(string(p))
	if strings.HasPrefix(line, "Review: ") {
		w.onID(strings.TrimPrefix(line, "Review: "))
	}
	return n, err
}

// TestReviewConfigRejectsAmbiguity не допускает опечатки и конкурирующие источники.
func TestReviewConfigRejectsAmbiguity(t *testing.T) {
	for _, input := range []string{`{"Reveiw":{}}`, `null`, `{"Review":{"Effort":"unknown"}}`, `{} {}`} {
		if _, err := reviewConfig("", input); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	if _, err := reviewConfig("file", "{}"); err == nil {
		t.Fatal("два источника")
	}
	cfg, err := reviewConfig("", `{"Review":{"Effort":"medium"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.Effort != "medium" || cfg.Context.Model != "gpt-6-sol" {
		t.Fatal(cfg)
	}
}

// TestReviewPersistsExecutable сохраняет явный runtime для повторов из браузера.
func TestReviewPersistsExecutable(t *testing.T) {
	root := t.TempDir()
	d := dependencies{reviewRun: func(_ context.Context, root, id, executable string) error {
		v, err := reviewstore.New(root).Load(id)
		if err != nil {
			return err
		}
		if executable != "/custom/codex" || v.Config.CodexExecutable != executable {
			t.Fatalf("runtime lost: %+v", v.Config)
		}
		return nil
	}}
	if err := reviewCommand(context.Background(), []string{"--prompt", "review", "--cwd", t.TempDir(), "--root", root, "--codex", "/custom/codex", "--no-ui"}, io.Discard, io.Discard, d); err != nil {
		t.Fatal(err)
	}
}

// TestReviewCLIErrorSurvivesUI сохраняет ошибку выполнения при доступном общем UI.
func TestReviewCLIErrorSurvivesUI(t *testing.T) {
	want := errors.New("ошибка агента")
	d := dependencies{reviewRun: func(context.Context, string, string, string) error { return want }}
	err := reviewCommand(context.Background(), []string{"--prompt", "review", "--root", t.TempDir(), "--cwd", t.TempDir(), "--no-ui"}, io.Discard, io.Discard, d)
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
}
