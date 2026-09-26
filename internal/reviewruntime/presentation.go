package reviewruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// presentation материализует Markdown и промпт исправления из того же графа
// доказательств, что отображает viewer. Агент не публикует отдельный пересказ.
func (s *stageSession) presentation(p reviewstore.Presentation) (string, error) {
	r, err := s.engine.Store.Load(s.id)
	if err != nil {
		return "", err
	}
	r.Presentation = p
	if err = reviewstore.Validate(r); err != nil {
		return "", err
	}
	if err = validateTours(r); err != nil {
		return "", err
	}
	prefix := fmt.Sprintf("artifacts/presentation/%d/%d", s.number, r.Revision)
	var all strings.Builder
	fmt.Fprintf(&all, "# %s\n\n%s\n\n", r.Title, p.Summary)
	overview, err := s.tourMarkdown(r, p.Overview)
	if err != nil {
		return "", err
	}
	all.WriteString(overview)
	paths := []string{}
	for i := range r.Findings {
		f := &r.Findings[i]
		var tour reviewstore.Tour
		for _, t := range p.FindingTours {
			if t.FindingID == f.ID {
				tour = t
				break
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<!-- lawa-review:%s:%s -->\n# [%s] %s\n\n%s\n\n## Воспроизведение\n%s\n\n## Последствия\n%s\n\n", r.ID, f.ID, f.Priority, f.Title, f.Explanation, f.Reproduction, f.Consequence)
		if len(tour.Steps) > 0 {
			md, e := s.tourMarkdown(r, tour)
			if e != nil {
				return "", e
			}
			b.WriteString(md)
			f.TourID = tour.ID
		} else {
			for _, a := range f.Anchors {
				md, e := s.anchorMarkdown(r, a)
				if e != nil {
					return "", e
				}
				b.WriteString(md)
			}
		}
		for _, receipt := range r.Publications {
			if receipt.FindingID == f.ID && receipt.State == "published" && receipt.ContentHash != fmt.Sprintf("%x", sha256.Sum256([]byte(b.String()))) {
				return "", errors.New("опубликованное доказательство отличается: сначала сверить и обновить внешний комментарий")
			}
		}
		f.MarkdownPath = fmt.Sprintf("%s/finding-%d.md", prefix, i+1)
		f.FixPromptPath = fmt.Sprintf("%s/fix-%d.md", prefix, i+1)
		if err = s.engine.Store.SaveArtifact(s.id, f.MarkdownPath, []byte(b.String())); err != nil {
			return "", err
		}
		fix := "Независимо проверь это замечание на актуальной ревизии проекта. Проследи доказательства, требования и сценарий. Если оно подтверждается, исправь причину и проверь регрессию; если нет, объясни фактами. Не выполняй merge.\n\n" + b.String()
		if err = s.engine.Store.SaveArtifact(s.id, f.FixPromptPath, []byte(fix)); err != nil {
			return "", err
		}
		all.WriteString("\n---\n\n" + b.String())
		paths = append(paths, f.ID+": "+f.MarkdownPath)
	}
	p.MarkdownPath = prefix + "/review.md"
	if err = s.engine.Store.SaveArtifact(s.id, p.MarkdownPath, []byte(all.String())); err != nil {
		return "", err
	}
	_, err = s.engine.Store.Update(s.id, func(current *reviewstore.Review) error {
		current.Presentation = p
		current.Findings = r.Findings
		return nil
	})
	return "Сохранён единый Markdown: " + p.MarkdownPath + "\nКомментарии для публикации (прочитай полные файлы):\n" + strings.Join(paths, "\n"), err
}

// validateTours не требует пути по коду при явно объяснённом отсутствии связи.
func validateTours(r reviewstore.Review) error {
	if r.Presentation.Overview.ID == "" || len(r.Presentation.Overview.Steps) == 0 {
		return errors.New("обязателен смысловой обзор изменений")
	}
	for _, t := range append([]reviewstore.Tour{r.Presentation.Overview}, r.Presentation.FindingTours...) {
		for _, step := range t.Steps {
			if len(step.Anchors) == 0 {
				return fmt.Errorf("шаг %s требует материалов или NoAnchorReason", step.ID)
			}
			for _, a := range step.Anchors {
				if a.FileID == "" && a.TaskID == "" && a.DesignID == "" && a.NoAnchorReason == "" {
					return errors.New("пустая привязка шага")
				}
			}
		}
	}
	for _, f := range r.Findings {
		found := false
		for _, t := range r.Presentation.FindingTours {
			if t.FindingID == f.ID && len(t.Steps) > 0 {
				found = true
			}
		}
		if !found {
			unbound := len(f.Anchors) > 0
			for _, a := range f.Anchors {
				if a.NoAnchorReason == "" || a.FileID != "" {
					unbound = false
				}
			}
			if !unbound {
				return fmt.Errorf("замечание %s требует маршрута доказательства", f.ID)
			}
		}
	}
	return nil
}

// tourMarkdown экспортирует шаги в смысловом порядке без приватной цепочки мыслей.
func (s *stageSession) tourMarkdown(r reviewstore.Review, t reviewstore.Tour) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", t.Title)
	for i, step := range t.Steps {
		fmt.Fprintf(&b, "### %d. %s\n\n%s\n\n", i+1, step.Title, step.Body)
		if step.ErrorLocation {
			b.WriteString("**Место ошибки**\n\n")
		}
		for _, a := range step.Anchors {
			md, err := s.anchorMarkdown(r, a)
			if err != nil {
				return "", err
			}
			b.WriteString(md)
		}
	}
	return b.String(), nil
}

// anchorMarkdown включает сам фрагмент: локальный artifact path не доступен
// будущему агенту из PR. Задачи и дизайн сохраняют внешние ссылки и версии.
func (s *stageSession) anchorMarkdown(r reviewstore.Review, a reviewstore.Anchor) (string, error) {
	var b strings.Builder
	if a.NoAnchorReason != "" {
		fmt.Fprintf(&b, "Связь с кодом отсутствует: %s\n\n", a.NoAnchorReason)
	}
	if a.FileID != "" {
		var f reviewstore.File
		for _, list := range [][]reviewstore.File{r.Context.Changes, r.Context.ProjectFiles, r.EvidenceFiles} {
			for _, file := range list {
				if file.ID == a.FileID {
					f = file
				}
			}
		}
		data, err := s.engine.Store.ReadArtifact(s.id, f.SnapshotPath)
		if err != nil {
			return "", err
		}
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		first := f.StartLine
		if first <= 0 {
			first = 1
		}
		start, end := a.StartLine, a.EndLine
		if start <= 0 {
			start = first
		}
		if end <= 0 {
			end = first + len(lines) - 1
		}
		if start < first || end < start || end >= first+len(lines) {
			return "", fmt.Errorf("диапазон %s:%d–%d вне сохранённого снимка", f.Path, start, end)
		}
		code := strings.Join(lines[start-first:end-first+1], "\n")
		fence := "```"
		for strings.Contains(code, fence) {
			fence += "`"
		}
		fmt.Fprintf(&b, "`%s:%d–%d` · ревизия `%s`\n\n%s\n%s\n%s\n\n", f.Path, start, end, f.Revision, fence, code, fence)
	}
	if a.TaskID != "" {
		for _, t := range r.Context.Tasks {
			if t.ID == a.TaskID {
				fmt.Fprintf(&b, "Требование: [%s — %s](%s)\n\n", t.ID, t.Title, t.URL)
				if t.BodyPath != "" {
					body, err := s.engine.Store.ReadArtifact(s.id, t.BodyPath)
					if err != nil {
						return "", err
					}
					fmt.Fprintf(&b, "<details><summary>Сохранённые требования</summary>\n\n%s\n\n</details>\n\n", body)
				}
				for _, comment := range t.Comments {
					body, err := s.engine.Store.ReadArtifact(s.id, comment.BodyPath)
					if err != nil {
						return "", err
					}
					fmt.Fprintf(&b, "<details><summary>Комментарий %s · %s</summary>\n\nИсточник: %s\n\n%s\n\n</details>\n\n", comment.Author, comment.CreatedAt, comment.URL, body)
				}
			}
		}
	}
	if a.DesignID != "" {
		for _, list := range [][]reviewstore.Design{r.Context.Designs, r.EvidenceDesigns} {
			for _, d := range list {
				if d.ID == a.DesignID {
					fmt.Fprintf(&b, "Дизайн: [%s](%s), версия %s. Сохранённый снимок: `%s`.\n\n", d.Title, d.SourceURL, d.Version, d.Path)
				}
			}
		}
	}
	return b.String(), nil
}

// publication хранит намерение до запроса и подтверждение после него. Повтор
// разрешён только с тем же finding/revision; planned не считается успехом.
func (s *stageSession) publication(p reviewstore.Publication) (string, error) {
	_, err := s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
		if r.ChangeURL == "" {
			return errors.New("локальное ревью не публикуется")
		}
		exists := false
		contentHash := ""
		for _, f := range r.Findings {
			if f.ID == p.FindingID && f.MarkdownPath != "" {
				exists = true
				data, err := s.engine.Store.ReadArtifact(s.id, f.MarkdownPath)
				if err != nil {
					return err
				}
				contentHash = fmt.Sprintf("%x", sha256.Sum256(data))
			}
		}
		if !exists {
			return errors.New("сначала подготовьте Markdown замечания")
		}
		p.ContentHash = contentHash
		if p.Revision == "" || p.Revision != r.SourceRevision {
			return errors.New("публикация требует сохранённую ревизию")
		}
		if p.State != "planned" && p.State != "published" && p.State != "failed" {
			return errors.New("некорректный статус публикации")
		}
		if p.State == "published" {
			u, e := url.Parse(p.URL)
			if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || p.ExternalID == "" {
				return errors.New("нужны подтверждённые URL и ExternalID")
			}
			now := time.Now().UTC()
			p.PublishedAt = &now
		}
		for i, old := range r.Publications {
			if old.FindingID == p.FindingID && old.Revision == p.Revision {
				if old.State == "published" && p.State != "published" {
					return errors.New("уже опубликовано; сначала проверь существующий комментарий")
				}
				r.Publications[i] = p
				return nil
			}
		}
		if p.State == "published" {
			return errors.New("перед публикацией необходимо сохранить planned")
		}
		r.Publications = append(r.Publications, p)
		return nil
	})
	return "Состояние публикации сохранено", err
}

// validate проверяет не только JSON, но и существование всех необходимых файлов.
func (s *stageSession) validate() error {
	r, err := s.engine.Store.Load(s.id)
	if err != nil {
		return err
	}
	paths := []string{}
	if strings.TrimSpace(r.Title) == "" {
		return errors.New("не заполнен заголовок review")
	}
	for _, activity := range r.Activities {
		if activity.Kind == "subagent" && activity.Stage == s.stage && activity.Attempt == s.number && (activity.State == "running" || activity.State == "pendingInit" || activity.State == "inProgress") {
			return fmt.Errorf("субагент %s ещё работает: дождитесь результата или явно остановите его", activity.ThreadID)
		}
	}
	if s.stage == reviewstore.ContextStage {
		if len(r.Context.Changes)+len(r.Context.ProjectFiles) == 0 {
			return errors.New("нет сохранённых файлов для проверки")
		}
		if len(r.Context.Changes) > 0 && r.Context.DiffPath == "" {
			return errors.New("изменения требуют сохранённого diff")
		}
	}
	for _, command := range r.Context.Commands {
		if command.OutputPath != "" {
			paths = append(paths, command.OutputPath)
		}
		if command.ScriptPath != "" {
			paths = append(paths, command.ScriptPath)
		}
	}
	if r.Context.DiffPath != "" {
		paths = append(paths, r.Context.DiffPath)
	}
	for _, t := range r.Context.Tasks {
		if t.BodyPath == "" {
			return errors.New("задача без сохранённого текста")
		}
		paths = append(paths, t.BodyPath)
		for _, c := range t.Comments {
			paths = append(paths, c.BodyPath)
		}
	}
	for _, list := range [][]reviewstore.File{r.Context.Changes, r.Context.ProjectFiles, r.EvidenceFiles} {
		for _, f := range list {
			paths = append(paths, f.SnapshotPath)
		}
	}
	for _, list := range [][]reviewstore.Design{r.Context.Designs, r.EvidenceDesigns} {
		for _, d := range list {
			paths = append(paths, d.Path)
			data, err := s.engine.Store.ReadArtifact(s.id, d.Path)
			if err != nil {
				return err
			}
			if !validDesignImage(data) {
				return fmt.Errorf("дизайн %s: требуется настоящий PNG/JPEG/GIF/WebP, а не текст или вёрстка", d.ID)
			}
		}
	}
	for _, f := range r.Findings {
		if err = validFinding(f); err != nil {
			return err
		}
	}
	if s.stage == reviewstore.ReviewStage && r.Verdict == "" {
		return errors.New("сохраните review_findings, даже если замечаний нет")
	}
	if s.stage == reviewstore.PresentationStage {
		if err = validateTours(r); err != nil {
			return err
		}
		paths = append(paths, r.Presentation.MarkdownPath)
		for _, f := range r.Findings {
			paths = append(paths, f.MarkdownPath, f.FixPromptPath)
			if r.ChangeURL != "" {
				published := false
				for _, p := range r.Publications {
					if p.FindingID == f.ID && p.Revision == r.SourceRevision && p.State == "published" {
						data, err := s.engine.Store.ReadArtifact(s.id, f.MarkdownPath)
						if err != nil {
							return err
						}
						if p.ContentHash != fmt.Sprintf("%x", sha256.Sum256(data)) {
							return errors.New("опубликованный Markdown отличается от результата")
						}
						published = true
					}
				}
				if !published {
					return fmt.Errorf("замечание %s ещё не опубликовано в PR/MR", f.ID)
				}
			}
		}
	}
	for _, p := range paths {
		if p == "" {
			return errors.New("материал не имеет сохранённого файла")
		}
		if _, err = s.engine.Store.ReadArtifact(s.id, p); err != nil {
			return fmt.Errorf("не доступен материал %s: %w", p, err)
		}
	}
	return nil
}

// validDesignImage проверяет растровый файл по содержимому, не расширению пути.
// Для WebP без дополнительной зависимости проверяется контейнер и вид чанка;
// SVG/HTML не принимаются, так как viewer показывает сохранённый снимок дизайна.
func validDesignImage(data []byte) bool {
	if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		return config.Width > 0 && config.Height > 0 && int64(config.Width)*int64(config.Height) <= 100_000_000
	}
	if len(data) < 30 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return false
	}
	size := uint64(binary.LittleEndian.Uint32(data[4:8])) + 8
	if size != uint64(len(data)) {
		return false
	}
	kind := string(data[12:16])
	return kind == "VP8 " || kind == "VP8L" || kind == "VP8X"
}
