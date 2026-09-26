package reviewruntime

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// Остановка или crash после записи промпта, но до регистрации попытки, оставляет
// сиротский immutable artifact. Retry обязан продолжить, сохранив этот файл.
func TestRetryAfterUnregisteredPrompt(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, stopped := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy=%v/stopped=%v", legacy, stopped), func(t *testing.T) {
				e, id := fixture(t)
				r, err := e.Store.Update(id, func(r *reviewstore.Review) error { r.State = reviewstore.Running; return nil })
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(e.Store.Root, id)
				prompt := stagePrompt(r, reviewstore.ContextStage, dir, filepath.Join(dir, "scratch", "context", "1"))
				path := attemptPromptPath(reviewstore.ContextStage, 1, prompt)
				if legacy {
					path = "artifacts/attempts/context/1/prompt.md"
				}
				if err = e.Store.SaveArtifact(id, path, []byte(prompt)); err != nil {
					t.Fatal(err)
				}
				if stopped {
					if _, err = e.Store.RequestStop(id); err != nil {
						t.Fatal(err)
					}
					if _, err = e.Store.Update(id, func(r *reviewstore.Review) error { r.State = reviewstore.Stopped; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				e.RunCodex = func(_ context.Context, c codex.Command) (codex.Result, error) {
					populate(t, c)
					return codex.Result{Status: "completed"}, nil
				}
				if err = e.Retry(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				after, err := e.Store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if after.State != reviewstore.Succeeded || len(after.Stages[0].Attempts) != 1 {
					t.Fatalf("retry не завершён: %+v", after.Stages)
				}
				attempt := after.Stages[0].Attempts[0]
				if attempt.PromptPath == path {
					t.Fatal("новый snapshot должен иметь собственный промпт")
				}
				if _, err := e.Store.ReadArtifact(id, attempt.PromptPath); err != nil {
					t.Fatal(err)
				}
				old, err := e.Store.ReadArtifact(id, path)
				if err != nil || string(old) != prompt {
					t.Fatal("история незарегистрированного промпта потеряна", err)
				}
			})
		}
	}
}
