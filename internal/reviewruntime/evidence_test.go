package reviewruntime

import (
	"context"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
	"strings"
	"testing"
)

// Полная фикстура request_changes проверяет паритет UI-контекста и Markdown.
func TestAuditFullFindingEvidence(t *testing.T) {
	e, id := fixture(t)
	e.RunCodex = func(ctx context.Context, c codex.Command) (codex.Result, error) {
		if strings.Contains(c.Text, `этап "context"`) {
			invoke(t, c, "review_artifact", artifactInput{Path: "artifacts/code", Text: "func submit() {\n send()\n}\n"})
			invoke(t, c, "review_artifact", artifactInput{Path: "artifacts/task", Text: "Добавить вход."})
			invoke(t, c, "review_artifact", artifactInput{Path: "artifacts/comment", Text: "REQUIREMENT_FROM_COMMENT: повторное нажатие блокируется до ответа."})
			invoke(t, c, "review_context", contextInput{Title: "Вход", SourceRevision: "abc", Context: reviewstore.Context{Tasks: []reviewstore.Task{{ID: "T1", Title: "Вход", URL: "https://tracker.example/t/1", BodyPath: "artifacts/task", Comments: []reviewstore.Comment{{ID: "c1", URL: "https://tracker.example/t/1#c1", BodyPath: "artifacts/comment"}}}}, ProjectFiles: []reviewstore.File{{ID: "code", Path: "login.go", SnapshotPath: "artifacts/code", StartLine: 1, EndLine: 3, Change: "context", Revision: "abc"}}}})
		} else if strings.Contains(c.Text, `этап "review"`) {
			invoke(t, c, "review_findings", findingsInput{Findings: []reviewstore.Finding{{ID: "f1", Priority: "P1", Title: "Дублирующий запрос", Explanation: "Нет блокировки повторного входа", Reproduction: "Два быстрых нажатия", Consequence: "Два запроса", Anchors: []reviewstore.Anchor{{FileID: "code", StartLine: 1, EndLine: 3, TaskID: "T1"}}}}})
		} else {
			step := reviewstore.TourStep{ID: "s1", Title: "Отправка", Body: "Повторное нажатие отправляет ещё один запрос.", Anchors: []reviewstore.Anchor{{FileID: "code", StartLine: 1, EndLine: 3, TaskID: "T1"}}}
			invoke(t, c, "review_presentation", reviewstore.Presentation{Overview: reviewstore.Tour{ID: "overview", Title: "Обзор", Steps: []reviewstore.TourStep{step}}, FindingTours: []reviewstore.Tour{{ID: "finding-tour", FindingID: "f1", Title: "Дублирование", Steps: []reviewstore.TourStep{step}}}})
		}
		invoke(t, c, "review_finish", finishInput{})
		return codex.Result{Status: "completed"}, nil
	}
	if err := e.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	r, err := e.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != reviewstore.Succeeded || r.Verdict != "request_changes" || len(r.Findings) != 1 || r.Findings[0].TourID == "" {
		t.Fatalf("неполный результат: %#v", r)
	}
	md, err := e.Store.ReadArtifact(id, r.Findings[0].MarkdownPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "REQUIREMENT_FROM_COMMENT") {
		t.Fatal("Markdown результата потерял сохранённое требование из комментария задачи")
	}
}
