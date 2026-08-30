package statusreport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/coordinator"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// rendererFunc позволяет тесту задать renderer без отдельной служебной структуры.
type rendererFunc func(context.Context, []byte) ([]byte, error)

// Render реализует production-интерфейс прямым вызовом тестовой функции.
func (f rendererFunc) Render(ctx context.Context, source []byte) ([]byte, error) {
	return f(ctx, source)
}

// TestDetailedReportLinksStatesAndEscaping проверяет локальный подробный отчёт:
// порядок и значения не меняются, известный чат получает точный Codex deeplink,
// пустой ID не подменяется, а путь текущего run кодируется для VS Code.
func TestDetailedReportLinksStatesAndEscaping(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run с пробелом")
	states := []scheduler.State{
		scheduler.Pending,
		scheduler.Starting,
		scheduler.Running,
		scheduler.Succeeded,
		scheduler.Failed,
		scheduler.Unknown,
		scheduler.State("future_state"),
	}
	status := coordinator.Status{WorkflowID: "review\nworkflow"}
	for index, state := range states {
		status.Steps = append(status.Steps, coordinator.StepStatus{
			ID:    "step-" + string(rune('a'+index)),
			State: state,
		})
	}
	status.Steps[2].ID = "running[unsafe]\n"
	status.Steps[2].CodexThreadID = "019c061e-4ea0-73e2-b1ef-523c2b469d3a"

	message := DetailedReport(status, runDir, Artifacts{
		SourcePath: filepath.Join(runDir, SourceFilename),
		ImagePath:  filepath.Join(runDir, ImageFilename),
	}, nil)
	if !strings.Contains(message, `[running\[unsafe\]\n](codex://threads/019c061e-4ea0-73e2-b1ef-523c2b469d3a) — running`) {
		t.Fatalf("нет безопасной ссылки на точный чат: %q", message)
	}
	if strings.Count(message, "(чат ещё не создан)") != len(states)-1 {
		t.Fatalf("пустые чаты не получили явную пометку: %q", message)
	}
	if strings.Count(message, "\n- ") != len(states) {
		t.Fatalf("список статуса содержит не каждый кубик ровно один раз: %q", message)
	}
	if !strings.Contains(message, "vscode://file/") || !strings.Contains(message, "run%20%D1%81%20%D0%BF%D1%80%D0%BE%D0%B1%D0%B5%D0%BB%D0%BE%D0%BC") {
		t.Fatalf("путь run не закодирован как VS Code deeplink: %q", message)
	}
	for _, state := range states {
		if !strings.Contains(message, "— "+string(state)) {
			t.Errorf("состояние %q потеряно или подменено: %q", state, message)
		}
	}
	if strings.ContainsAny(message, "\r\x1b") || strings.Contains(message, "review\nworkflow") {
		t.Fatalf("управляющие символы изменили структуру сообщения: %q", message)
	}
}

// TestSummaryUsesCompactUserCategories фиксирует короткий формат из 15 шагов и
// гарантирует, что дорогие ID кубиков, чатов и картинки не возвращаются в stdout.
func TestSummaryUsesCompactUserCategories(t *testing.T) {
	status := coordinator.Status{RunID: "run-1", WorkflowID: "report"}
	for index := range 5 {
		status.Steps = append(status.Steps, coordinator.StepStatus{ID: fmt.Sprintf("ready-%d", index), State: scheduler.Succeeded})
	}
	for index := range 6 {
		state := scheduler.Running
		if index == 0 {
			state = scheduler.Starting
		}
		status.Steps = append(status.Steps, coordinator.StepStatus{ID: fmt.Sprintf("running-%d", index), CodexThreadID: "chat", State: state})
	}
	for index := range 4 {
		status.Steps = append(status.Steps, coordinator.StepStatus{ID: fmt.Sprintf("waiting-%d", index), State: scheduler.Pending})
	}

	summary := Summary(status, t.TempDir(), nil)
	if !strings.Contains(summary, "Всего: 15, готово: 5, работает: 6, ожидают: 4.") || !strings.Contains(summary, "vscode://file/") {
		t.Fatalf("неверная краткая статистика: %q", summary)
	}
	for _, forbidden := range []string{"ready-0", "codex://threads/", "workflow-status.png", "PlantUML"} {
		if strings.Contains(summary, forbidden) {
			t.Errorf("краткая сводка содержит лишнюю деталь %q: %q", forbidden, summary)
		}
	}

	status.Steps = append(status.Steps, coordinator.StepStatus{ID: "failed", State: scheduler.Failed})
	if warning := Summary(status, t.TempDir(), errors.New("renderer failed")); !strings.Contains(warning, "требуют внимания: 1") || !strings.Contains(warning, "renderer failed") {
		t.Fatalf("проблемное состояние или диагностика скрыты: %q", warning)
	}
}

// TestPlantUMLContainsGraphStatesAndLegend фиксирует схему: зависимости идут от
// родителя к ребёнку, подписи содержат исходное состояние, а ожидаемые состояния
// различаются стабильными цветами. Будущее значение остаётся текстом и серым.
func TestPlantUMLContainsGraphStatesAndLegend(t *testing.T) {
	status := coordinator.Status{
		WorkflowID: "review",
		Steps: []coordinator.StepStatus{
			{ID: "scope", State: scheduler.Succeeded},
			{ID: "criterion", State: scheduler.Running, DependsOn: []string{"scope"}},
			{ID: "future", State: scheduler.State("paused_by_host"), DependsOn: []string{"criterion"}},
		},
	}
	source, err := PlantUML(status)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, fragment := range []string{
		"@startuml",
		`rectangle "scope\nsucceeded" as step_0 #86EFAC`,
		`rectangle "criterion\nrunning" as step_1 #93C5FD`,
		`rectangle "future\npaused_by_host" as step_2 #D1D5DB`,
		"step_0 --> step_1",
		"step_1 --> step_2",
		"|<#F3F4F6> | pending |",
		"|<#FDE68A> | starting |",
		"|<#93C5FD> | running |",
		"|<#86EFAC> | succeeded |",
		"|<#FCA5A5> | failed |",
		"|<#D1D5DB> | unknown |",
		"@enduml",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("в PlantUML отсутствует %q:\n%s", fragment, text)
		}
	}
}

// TestWriteReportPublishesOneSnapshotAndRemovesStaleImage проверяет два
// обновления без реального PlantUML. Markdown, PNG и source относятся к одному
// Status; при следующем отказе подробный текст и source обновляются, а старый PNG
// исчезает и не остаётся подключённым в отчёте.
func TestWriteReportPublishesOneSnapshotAndRemovesStaleImage(t *testing.T) {
	runDir := t.TempDir()
	status := coordinator.Status{
		WorkflowID: "flow",
		Steps:      []coordinator.StepStatus{{ID: "cube", State: scheduler.Running}},
	}
	var renderedSource []byte
	renderer := rendererFunc(func(_ context.Context, source []byte) ([]byte, error) {
		renderedSource = append([]byte(nil), source...)
		return append(append([]byte(nil), pngSignature...), []byte("same-snapshot")...), nil
	})
	artifacts, err := WriteReport(t.Context(), runDir, status, renderer)
	if err != nil {
		t.Fatal(err)
	}
	savedSource, sourceErr := os.ReadFile(artifacts.SourcePath)
	savedImage, imageErr := os.ReadFile(artifacts.ImagePath)
	savedReport, reportErr := os.ReadFile(artifacts.ReportPath)
	if sourceErr != nil || imageErr != nil || reportErr != nil || !bytes.Equal(savedSource, renderedSource) || !bytes.HasPrefix(savedImage, pngSignature) ||
		!strings.Contains(string(savedReport), "![Текущая схема workflow](workflow-status.png)") {
		t.Fatalf("source и PNG опубликованы несогласованно: source=%v image=%v", sourceErr, imageErr)
	}

	status.Steps[0].State = scheduler.Failed
	broken := errors.New("renderer unavailable")
	artifacts, err = WriteReport(t.Context(), runDir, status, rendererFunc(func(context.Context, []byte) ([]byte, error) {
		return nil, broken
	}))
	if !errors.Is(err, broken) || artifacts.ReportPath == "" || artifacts.SourcePath == "" || artifacts.ImagePath != "" {
		t.Fatalf("ошибка renderer потеряна или объявлен старый PNG: %+v, %v", artifacts, err)
	}
	savedSource, sourceErr = os.ReadFile(artifacts.SourcePath)
	_, imageErr = os.Stat(filepath.Join(runDir, ImageFilename))
	if sourceErr != nil || !strings.Contains(string(savedSource), `cube\nfailed`) || !errors.Is(imageErr, os.ErrNotExist) {
		t.Fatalf("после отказа осталась старая схема: source=%v image=%v", sourceErr, imageErr)
	}
	savedReport, reportErr = os.ReadFile(artifacts.ReportPath)
	if reportErr != nil || !strings.Contains(string(savedReport), "cube — failed") ||
		!strings.Contains(string(savedReport), "Текстовый статус выше остаётся актуальным") || strings.Contains(string(savedReport), "![Текущая схема workflow]") {
		t.Fatalf("диагностика скрыла текст или предложила старый PNG: %v, %q", reportErr, savedReport)
	}
}

// TestPlantUMLRejectsBrokenDependencies не позволяет нарисовать правдоподобную,
// но неполную схему для повреждённого согласованного снимка.
func TestPlantUMLRejectsBrokenDependencies(t *testing.T) {
	_, err := PlantUML(coordinator.Status{Steps: []coordinator.StepStatus{{
		ID: "child", State: scheduler.Pending, DependsOn: []string{"missing"},
	}}})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("повреждённая зависимость не объяснена: %v", err)
	}
}
