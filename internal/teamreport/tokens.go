package teamreport

import "github.com/stray-live-pixel/Lawa/internal/runstore"

// turnTokens вычисляет разность накопленных счётчиков только при известной
// базе: новый thread либо непосредственно предыдущий завершённый turn того же
// сотрудника. Пропущенный usage, рестарт без конца, сброс счётчика или чужой
// thread дают null. Это наблюдаемые токены, не обещание полной тарификации.
func turnTokens(metrics *runstore.TeamMetrics, current *runstore.TeamExecution) *runstore.TeamTokenCounts {
	if current.Usage == nil || current.Usage.Total == nil {
		return nil
	}
	if current.NewThread {
		return current.Usage.Total
	}
	var previous *runstore.TeamExecution
	for _, e := range metrics.Executions {
		if e.ActorID == current.ActorID && e.Attempt < current.Attempt && (previous == nil || e.Attempt > previous.Attempt) {
			previous = e
		}
	}
	if previous == nil || previous.ThreadID != current.ThreadID || previous.FinishedAt == nil || previous.Usage == nil || previous.Usage.Total == nil {
		return nil
	}
	a, b := current.Usage.Total, previous.Usage.Total
	result := &runstore.TeamTokenCounts{Total: difference(a.Total, b.Total), Input: difference(a.Input, b.Input), CachedInput: difference(a.CachedInput, b.CachedInput), Output: difference(a.Output, b.Output), ReasoningOutput: difference(a.ReasoningOutput, b.ReasoningOutput), CacheWriteInput: difference(a.CacheWriteInput, b.CacheWriteInput)}
	if !result.Valid() {
		return nil
	}
	return result
}

// difference не подменяет отсутствующий счётчик нулём и не принимает откат.
func difference(a, b *int64) *int64 {
	if a == nil || b == nil || *a < *b {
		return nil
	}
	n := *a - *b
	return &n
}
