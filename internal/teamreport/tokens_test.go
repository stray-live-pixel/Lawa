package teamreport

import (
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// Накопленные 100 и 140 токенов дают 100 и 40 на два turn, а не 240.
// Отсутствующий baseline после старого запуска/аварии остаётся неизвестным.
func TestTokensUseConsecutiveKnownBaseline(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	now := time.Now()
	first := &runstore.TeamExecution{ActorID: "a", Attempt: 1, ThreadID: "thread", NewThread: true, FinishedAt: &now, Usage: &runstore.TeamTokenUsage{Total: &runstore.TeamTokenCounts{Input: n(100)}}}
	second := &runstore.TeamExecution{ActorID: "a", Attempt: 2, ThreadID: "thread", Usage: &runstore.TeamTokenUsage{Total: &runstore.TeamTokenCounts{Input: n(140)}}}
	metrics := &runstore.TeamMetrics{Executions: []*runstore.TeamExecution{first, second}}
	if got := turnTokens(metrics, first); got == nil || *got.Input != 100 || got.Output != nil {
		t.Fatal(got)
	}
	if got := turnTokens(metrics, second); got == nil || *got.Input != 40 || got.Output != nil {
		t.Fatal(got)
	}
	first.FinishedAt = nil
	if turnTokens(metrics, second) != nil {
		t.Fatal("usage после неизвестного окончания")
	}
	first.FinishedAt = &now
	second.Usage.Total.Input = n(90)
	if turnTokens(metrics, second) != nil {
		t.Fatal("откат счётчика")
	}
	first.Usage = nil
	if turnTokens(metrics, second) != nil {
		t.Fatal("пропущенный usage стал нулём")
	}
}
