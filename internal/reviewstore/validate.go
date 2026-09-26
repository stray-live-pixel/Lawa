package reviewstore

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ValidateConfig проверяет входы до первого запуска; тарифы не подменяются нулём.
func ValidateConfig(c Config) error {
	if strings.ContainsRune(c.CodexExecutable, 0) || !utf8.ValidString(c.CodexExecutable) {
		return fmt.Errorf("неверное имя исполняемого файла Codex")
	}
	for _, a := range []AgentConfig{c.Context, c.Review, c.Presentation} {
		if !validText(a.Model) {
			return fmt.Errorf("модель этапа обязательна")
		}
		switch a.Effort {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		default:
			return fmt.Errorf("неизвестный effort %q", a.Effort)
		}
	}
	if !finite(c.RublesPerDollar) || c.RublesPerDollar <= 0 {
		return fmt.Errorf("курс должен быть положительным числом")
	}
	for model, p := range c.Prices {
		if !validText(model) || !finite(p.Input) || !finite(p.CachedInput) || !finite(p.Output) || p.Input < 0 || p.CachedInput < 0 || p.Output < 0 {
			return fmt.Errorf("неверный тариф %q", model)
		}
	}
	return nil
}

// MergeConfig последовательно накладывает частичные пользовательские настройки.
// Для завершения разрешения вызывающий использует ResolveConfig и ValidateConfig.
func MergeConfig(base, override Config) Config {
	if override.CodexExecutable != "" {
		base.CodexExecutable = override.CodexExecutable
	}
	for _, p := range []struct {
		to   *AgentConfig
		from AgentConfig
	}{{&base.Context, override.Context}, {&base.Review, override.Review}, {&base.Presentation, override.Presentation}} {
		if p.from.Model != "" {
			p.to.Model = p.from.Model
		}
		if p.from.Effort != "" {
			p.to.Effort = p.from.Effort
		}
	}
	if override.RublesPerDollar != 0 {
		base.RublesPerDollar = override.RublesPerDollar
	}
	if override.Prices != nil {
		prices := map[string]Pricing{}
		for k, v := range base.Prices {
			prices[k] = v
		}
		for k, v := range override.Prices {
			prices[k] = v
		}
		base.Prices = prices
	}
	return base
}

// Validate защищает данные, которые затем отображаются UI и передаются агентам.
// Пустые результаты допустимы во время сбора; завершение проверяет исполнитель.
func Validate(r Review) error {
	if r.Version != 1 || r.Kind != "review" || !ValidID(r.ID) || !filepath.IsAbs(r.CWD) || strings.ContainsRune(r.CWD, 0) || !validText(r.Prompt) || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) || r.Revision == 0 {
		return fmt.Errorf("неверные обязательные поля review")
	}
	if !validState(r.State) || !validStage(r.CurrentStage) {
		return fmt.Errorf("неизвестное состояние или этап")
	}
	if e := ValidateConfig(r.Config); e != nil {
		return e
	}
	if len(r.Stages) != 3 {
		return fmt.Errorf("review должен содержать три этапа")
	}
	for i, id := range []StageID{ContextStage, ReviewStage, PresentationStage} {
		st := r.Stages[i]
		if st.ID != id || !validState(st.State) {
			return fmt.Errorf("неверный порядок этапов")
		}
		for j, a := range st.Attempts {
			if a.Number != j+1 || !validState(a.State) || a.StartedAt.IsZero() {
				return fmt.Errorf("неверная попытка этапа %s", id)
			}
			if a.FinishedAt != nil && a.FinishedAt.Before(a.StartedAt) {
				return fmt.Errorf("окончание попытки раньше начала")
			}
			for _, p := range []string{a.PromptPath, a.OutputPath} {
				if e := optionalArtifact(p); e != nil {
					return e
				}
			}
			if e := ValidateUsage(a.Usage); e != nil {
				return e
			}
			for _, group := range a.ThreadUsage {
				if !validText(group.ThreadID) || !validText(group.Model) {
					return fmt.Errorf("группа usage требует threadId и модель")
				}
				if e := ValidateUsage(group.Usage); e != nil {
					return e
				}
			}
		}
	}
	if r.Verdict != "" && r.Verdict != "approve" && r.Verdict != "request_changes" {
		return fmt.Errorf("неизвестный вердикт")
	}
	taskIDs := map[string]bool{}
	for _, t := range r.Context.Tasks {
		if !validText(t.ID) || taskIDs[t.ID] {
			return fmt.Errorf("неверный/повторный ID задачи")
		}
		taskIDs[t.ID] = true
		if e := optionalArtifact(t.BodyPath); e != nil {
			return e
		}
		for _, c := range t.Comments {
			if e := optionalArtifact(c.BodyPath); e != nil {
				return e
			}
		}
	}
	fileIDs := map[string]bool{}
	for _, list := range [][]File{r.Context.Changes, r.Context.ProjectFiles, r.EvidenceFiles} {
		for _, f := range list {
			if !validText(f.ID) || fileIDs[f.ID] || !validText(f.Path) || f.StartLine < 0 || f.EndLine < 0 || f.StartLine > 0 && f.EndLine < f.StartLine || f.AddedLines < 0 || f.DeletedLines < 0 {
				return fmt.Errorf("неверный файл %q", f.ID)
			}
			fileIDs[f.ID] = true
			for _, line := range f.LineChanges {
				if line.StartLine < 1 || line.EndLine < line.StartLine {
					return fmt.Errorf("неверный диапазон diff")
				}
				switch line.Kind {
				case "added", "deleted", "context":
				default:
					return fmt.Errorf("неверный тип строки diff")
				}
			}
			switch f.Change {
			case "", "added", "deleted", "modified", "renamed", "context":
			default:
				return fmt.Errorf("неизвестный вид изменения %q", f.Change)
			}
			if e := optionalArtifact(f.SnapshotPath); e != nil {
				return e
			}
		}
	}
	designIDs := map[string]bool{}
	for _, list := range [][]Design{r.Context.Designs, r.EvidenceDesigns} {
		for _, d := range list {
			if !validText(d.ID) || designIDs[d.ID] {
				return fmt.Errorf("неверный/повторный ID дизайна")
			}
			designIDs[d.ID] = true
			if e := optionalArtifact(d.Path); e != nil {
				return e
			}
		}
	}
	for _, c := range r.Context.Commands {
		for _, p := range []string{c.ScriptPath, c.OutputPath} {
			if e := optionalArtifact(p); e != nil {
				return e
			}
		}
	}
	if e := optionalArtifact(r.Context.DiffPath); e != nil {
		return e
	}
	st := r.Context.Stats
	if st.AddedLines < 0 || st.DeletedLines < 0 || st.AddedFiles < 0 || st.DeletedFiles < 0 || st.ModifiedFiles < 0 {
		return fmt.Errorf("отрицательная статистика изменений")
	}
	findings := map[string]bool{}
	for _, f := range r.Findings {
		if !validText(f.ID) || findings[f.ID] || !validText(f.Title) {
			return fmt.Errorf("неверное замечание %q", f.ID)
		}
		findings[f.ID] = true
		switch f.Priority {
		case "P0", "P1", "P2", "P3":
		default:
			return fmt.Errorf("неизвестный приоритет %q", f.Priority)
		}
		for _, p := range []string{f.FixPromptPath, f.MarkdownPath} {
			if e := optionalArtifact(p); e != nil {
				return e
			}
		}
		for _, a := range f.Anchors {
			if e := validateAnchor(a, fileIDs, taskIDs, designIDs); e != nil {
				return e
			}
		}
	}
	tours := append([]Tour{r.Presentation.Overview}, r.Presentation.FindingTours...)
	tourIDs := map[string]bool{}
	for _, t := range tours {
		if t.ID == "" && len(t.Steps) == 0 {
			continue
		}
		if !validText(t.ID) || tourIDs[t.ID] {
			return fmt.Errorf("неверный ID экскурсии")
		}
		tourIDs[t.ID] = true
		if t.FindingID != "" && !findings[t.FindingID] {
			return fmt.Errorf("экскурсия ссылается на неизвестное замечание")
		}
		steps := map[string]bool{}
		for _, s := range t.Steps {
			if !validText(s.ID) || steps[s.ID] || !validText(s.Title) || !validText(s.Body) {
				return fmt.Errorf("неверный шаг экскурсии")
			}
			steps[s.ID] = true
			for _, a := range s.Anchors {
				if e := validateAnchor(a, fileIDs, taskIDs, designIDs); e != nil {
					return e
				}
			}
		}
	}
	for _, a := range r.Activities {
		if e := optionalArtifact(a.DocumentPath); e != nil {
			return e
		}
	}
	if e := optionalArtifact(r.Presentation.MarkdownPath); e != nil {
		return e
	}
	return nil
}

// ValidateUsage не разрешает отрицательные показатели или кэш сверх общего input.
func ValidateUsage(u Usage) error {
	for _, n := range []*int64{u.InputTokens, u.CachedInputTokens, u.OutputTokens} {
		if n != nil && *n < 0 {
			return fmt.Errorf("отрицательное usage")
		}
	}
	if u.CachedInputTokens != nil && u.InputTokens != nil && *u.CachedInputTokens > *u.InputTokens {
		return fmt.Errorf("cache превышает input")
	}
	if u.CostUSD != nil && (!finite(*u.CostUSD) || *u.CostUSD < 0) {
		return fmt.Errorf("неверная стоимость")
	}
	return nil
}

// PriceUsage возвращает API-эквивалент только при полной разбивке и известном
// тарифе. Кэш вычитается из общего input; повторные попытки суммируются отдельно.
func PriceUsage(u Usage, p *Pricing) Usage {
	u.CostUSD = nil
	u.Complete = false
	if ValidateUsage(u) != nil || p == nil || u.InputTokens == nil || u.CachedInputTokens == nil || u.OutputTokens == nil {
		return u
	}
	cost := (float64(*u.InputTokens-*u.CachedInputTokens)*p.Input + float64(*u.CachedInputTokens)*p.CachedInput + float64(*u.OutputTokens)*p.Output) / 1e6
	if !finite(cost) || cost < 0 {
		return u
	}
	u.CostUSD = &cost
	u.Complete = true
	return u
}
func validateAnchor(a Anchor, files, tasks, designs map[string]bool) error {
	if a.StartLine < 0 || a.EndLine < 0 || a.StartLine > 0 && a.EndLine < a.StartLine {
		return fmt.Errorf("неверный диапазон привязки")
	}
	if a.FileID != "" && !files[a.FileID] || a.TaskID != "" && !tasks[a.TaskID] || a.DesignID != "" && !designs[a.DesignID] {
		return fmt.Errorf("привязка ссылается на неизвестный материал")
	}
	if a.FileID == "" && (a.StartLine != 0 || a.EndLine != 0) {
		return fmt.Errorf("строки требуют файла")
	}
	return nil
}
func validStage(s StageID) bool {
	return s == ContextStage || s == ReviewStage || s == PresentationStage
}
func validState(s State) bool {
	switch s {
	case Pending, Running, Succeeded, Failed, Stopped, Interrupted:
		return true
	}
	return false
}
func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }
func validText(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && !strings.ContainsRune(s, 0)
}
func optionalArtifact(path string) error {
	if path == "" {
		return nil
	}
	return validArtifact(path)
}
func validArtifact(path string) error {
	if !utf8.ValidString(path) || strings.ContainsAny(path, "\\\x00") || !strings.HasPrefix(path, "artifacts/") || filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, "//") {
		return fmt.Errorf("небезопасный путь артефакта %q", path)
	}
	for _, p := range strings.Split(path, "/") {
		if p == "" || p == "." || p == ".." {
			return fmt.Errorf("небезопасный путь артефакта %q", path)
		}
	}
	return nil
}
