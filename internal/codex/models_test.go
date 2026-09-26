package codex

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestListModels читает все страницы каталога без thread/start и модельных turn.
func TestListModels(t *testing.T) {
	t.Setenv("LAWA_TEST_CODEX_SERVER", "models")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := ListModels(ctx, Connection{Executable: executable, CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Model != "model-a" || models[1].Model != "model-b" || models[0].SupportedReasoningEfforts[0].ReasoningEffort != "high" {
		t.Fatalf("%+v", models)
	}
}
