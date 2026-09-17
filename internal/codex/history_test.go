package codex

import (
	"os"
	"testing"
)

// Настоящий stdio fixture отклоняет неожиданные методы: ReadTurns должен
// только читать, проверять проект/ID и сохранять секундные timestamps.
func TestReadTurns(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"inspect:completed", "inspect:wrong-thread", "inspect:wrong-cwd"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("LAWA_TEST_CODEX_SERVER", scenario)
			observer, err := OpenObserver(t.Context(), Connection{Executable: binary, CWD: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close()
			turns, err := observer.ReadTurns("thread-1")
			if scenario != "inspect:completed" {
				if err == nil {
					t.Fatal("не проверена идентичность чата")
				}
				return
			}
			if err != nil || len(turns) != 1 || turns[0].StartedAt == nil || *turns[0].StartedAt != 100 || turns[0].CompletedAt == nil || *turns[0].CompletedAt != 110 {
				t.Fatalf("%+v %v", turns, err)
			}
		})
	}
}
