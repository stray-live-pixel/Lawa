// Package statusreport превращает один снимок координатора в короткую чат-сводку,
// подробный локальный Markdown и PlantUML-артефакты. Пакет ничего не читает из run:
// вызывающий код передаёт уже согласованные состояния и зависимости, поэтому все
// представления одного обновления не могут относиться к разным версиям meta.json.
package statusreport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/stray-live-pixel/Lawa/internal/coordinator"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

const (
	// ReportFilename, SourceFilename и ImageFilename — стабильные имена последнего
	// снимка в run. Временные файлы публикуются через rename, поэтому пользователь
	// не увидит частично записанный Markdown, source или PNG.
	ReportFilename = "workflow-status.md"
	SourceFilename = "workflow-status.puml"
	ImageFilename  = runstore.StatusImageFilename
)

// pngSignature отделяет готовое изображение от текстовой ошибки, которую
// некоторые command-line обёртки могут вернуть в stdout с нулевым кодом.
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// Renderer отделяет построение PlantUML source от локального процесса рендера.
// Production использует CommandRenderer, а тесты возвращают известный PNG без
// установки Java, Graphviz или PlantUML.
type Renderer interface {
	Render(context.Context, []byte) ([]byte, error)
}

// CommandRenderer запускает локальный PlantUML в pipe-режиме: source передаётся
// через stdin, PNG возвращается через stdout. Так renderer не получает пути run
// и не может самостоятельно перезаписать сохранённые входы workflow.
type CommandRenderer struct {
	Executable string
	Timeout    time.Duration
}

// Render ограничивает зависший renderer по времени и сохраняет stderr в ошибке.
// Вызывающий код считает эту ошибку нефатальной для workflow.
func (r CommandRenderer) Render(ctx context.Context, source []byte) ([]byte, error) {
	executable := strings.TrimSpace(r.Executable)
	if executable == "" {
		executable = "plantuml"
	}
	if r.Timeout <= 0 {
		r.Timeout = 30 * time.Second
	}
	renderContext, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	command := exec.CommandContext(renderContext, executable, "-pipe")
	// Даже после текстового экранирования запрещаем renderer читать локальные
	// файлы и сеть через include/img. Наследуем остальное окружение процесса,
	// заменяя только официальный профиль безопасности PlantUML.
	command.Env = append(os.Environ(), "PLANTUML_SECURITY_PROFILE=SANDBOX")
	command.Stdin = bytes.NewReader(source)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		details := strings.TrimSpace(stderr.String())
		if len(details) > 4096 {
			details = details[:4096] + "…"
		}
		if details != "" {
			return nil, fmt.Errorf("запустить %q -pipe: %w: %s", executable, err, details)
		}
		return nil, fmt.Errorf("запустить %q -pipe: %w", executable, err)
	}
	return stdout.Bytes(), nil
}

// Artifacts перечисляет только успешно опубликованные файлы. ReportPath и
// SourcePath могут быть заполнены без ImagePath, если локальный renderer сломан:
// подробный текст тогда остаётся доступен и объясняет отсутствие картинки.
type Artifacts struct {
	ReportPath string
	SourcePath string
	ImagePath  string
}

// WriteReport обновляет подробный Markdown и визуализацию одного Status. Сначала
// публикуются source и PNG, затем Markdown со ссылками на уже готовые файлы. При
// отказе renderer новый отчёт всё равно сохраняется, а прежний PNG удаляется:
// пользователь не примет старую картинку за текущую. Ошибки не меняют meta.json,
// workflow.json или память кубиков и возвращаются вызывающему коду для краткой
// диагностики в чат-сводке.
func WriteReport(ctx context.Context, runDir string, status coordinator.Status, renderer Renderer) (Artifacts, error) {
	if !filepath.IsAbs(runDir) {
		return Artifacts{}, fmt.Errorf("папка run должна быть абсолютной: %q", runDir)
	}
	artifacts, visualizationErr := writeVisualization(ctx, runDir, status, renderer)
	reportPath := filepath.Join(runDir, ReportFilename)
	report := DetailedReport(status, runDir, artifacts, visualizationErr)
	if err := writeAtomic(reportPath, []byte(report)); err != nil {
		cleanupErr := removeArtifact(reportPath)
		return artifacts, errors.Join(visualizationErr, fmt.Errorf("сохранить подробный Markdown-отчёт: %w", err), cleanupErr)
	}
	artifacts.ReportPath = reportPath
	return artifacts, visualizationErr
}

// writeVisualization сохраняет source и PNG одного снимка. Source остаётся при
// ошибке renderer для диагностики, а старый PNG удаляется до публикации Markdown.
func writeVisualization(ctx context.Context, runDir string, status coordinator.Status, renderer Renderer) (Artifacts, error) {
	sourcePath := filepath.Join(runDir, SourceFilename)
	imagePath := filepath.Join(runDir, ImageFilename)
	source, err := PlantUML(status)
	if err != nil {
		// Повреждённая структура тоже не должна оставлять картинку прежнего
		// корректного снимка под стабильным именем.
		return Artifacts{}, errors.Join(err, removeArtifact(imagePath), removeArtifact(sourcePath))
	}
	if err = writeAtomic(sourcePath, source); err != nil {
		cleanupErr := removeArtifact(imagePath)
		return Artifacts{}, errors.Join(fmt.Errorf("сохранить PlantUML source: %w", err), cleanupErr)
	}
	artifacts := Artifacts{SourcePath: sourcePath}
	if renderer == nil {
		err = errors.New("локальный renderer PlantUML не настроен")
	} else {
		var image []byte
		image, err = renderer.Render(ctx, source)
		if err == nil && !bytes.HasPrefix(image, pngSignature) {
			err = errors.New("renderer PlantUML вернул данные без PNG-сигнатуры")
		}
		if err == nil {
			err = writeAtomic(imagePath, image)
			if err != nil {
				err = fmt.Errorf("сохранить PNG PlantUML: %w", err)
			}
		}
	}
	if err != nil {
		return artifacts, errors.Join(err, removeArtifact(imagePath))
	}
	artifacts.ImagePath = imagePath
	return artifacts, nil
}

// DetailedReport строит содержимое локального workflow-status.md. Для legacy
// каждый кубик появляется один раз, для v4 каждое посещение — отдельной строкой
// в порядке meta.json. Пустой codexThreadId никогда не подменяется другим чатом.
func DetailedReport(status coordinator.Status, runDir string, artifacts Artifacts, visualizationErr error) string {
	var message strings.Builder
	waitingForCapacity := stringSet(status.WaitingForCapacity)
	fmt.Fprintf(&message, "# Статус workflow \"%s\"\n\n", markdownText(status.WorkflowID))
	for _, step := range status.Steps {
		labelText, capacityKey := step.ID, step.ID
		if isAgentStatus(status) {
			labelText, capacityKey = fmt.Sprintf("%s#%d", step.ID, step.Visit), step.VisitID
		}
		label := markdownText(labelText)
		if step.CodexThreadID != "" {
			label = fmt.Sprintf("[%s](%s)", label, codexThreadURL(step.CodexThreadID))
		}
		fmt.Fprintf(&message, "- %s — %s", label, markdownText(string(step.State)))
		if step.CodexThreadID == "" {
			message.WriteString(" (чат ещё не создан)")
		}
		if waitingForCapacity[capacityKey] {
			message.WriteString(" (ждёт свободный слот общего лимита)")
		}
		message.WriteByte('\n')
		if isAgentStatus(status) {
			writeAgentVisitDetails(&message, step)
		}
	}
	if isAgentStatus(status) && status.Terminal {
		message.WriteByte('\n')
		fmt.Fprintf(&message, "Run %s завершён: %s.\n", markdownText(status.RunID), markdownText(string(status.RunState)))
		if status.StopVisitID != "" {
			fmt.Fprintf(&message, "Остановившее посещение: %s.\n", markdownText(status.StopVisitID))
		}
		if status.StopReason != "" {
			fmt.Fprintf(&message, "Причина остановки: %s\n", markdownText(status.StopReason))
		}
	} else if status.Complete {
		message.WriteByte('\n')
		fmt.Fprintf(&message, "Run %s успешно завершён.\n", markdownText(status.RunID))
	}
	fmt.Fprintf(&message, "\n[Открыть папку run в VS Code](%s)\n", vscodeFolderURL(runDir))
	if visualizationErr == nil && artifacts.SourcePath != "" && artifacts.ImagePath != "" {
		fmt.Fprintf(&message, "\n![Текущая схема workflow](%s)\n\n[PlantUML source](%s)\n",
			ImageFilename, SourceFilename)
	} else {
		message.WriteString("\nСхема PlantUML не обновлена")
		if visualizationErr != nil {
			fmt.Fprintf(&message, ": %s", safeDiagnostic(visualizationErr.Error()))
		}
		message.WriteString(". Текстовый статус выше остаётся актуальным.\n")
		if artifacts.SourcePath != "" {
			fmt.Fprintf(&message, "Актуальный PlantUML source: %s\n", markdownText(artifacts.SourcePath))
		}
	}
	message.WriteByte('\n')
	return message.String()
}

// writeAgentVisitDetails раскрывает durable-поля одного посещения отдельными
// строками. Пропущенные ключи остаются свойством решения: отчёт не создаёт для
// них фиктивные посещения и не приписывает состояние целевым step.
func writeAgentVisitDetails(message *strings.Builder, step coordinator.StepStatus) {
	fmt.Fprintf(message, "  - visitId: %s; итерация: %d; попытка: %d\n",
		markdownText(step.VisitID), step.Iteration, step.Attempt)
	if trigger := visitTriggerText(step); trigger != "" {
		fmt.Fprintf(message, "  - причина запуска: %s\n", markdownText(trigger))
	}
	if step.MaxVisits != nil {
		fmt.Fprintf(message, "  - предел: maxVisits=%d", *step.MaxVisits)
		if step.OnLimit != nil {
			fmt.Fprintf(message, "; onLimit=%s", markdownText(string(*step.OnLimit)))
		}
		message.WriteByte('\n')
	}
	if step.TechnicalError != "" {
		fmt.Fprintf(message, "  - техническая ошибка: %s\n", markdownText(step.TechnicalError))
	}
	if step.Decision != nil {
		fmt.Fprintf(message, "  - решение: %s; применено: %t\n", markdownText(step.Decision.Key), step.Decision.Applied)
		if step.Decision.Explanation != "" {
			fmt.Fprintf(message, "  - объяснение решения: %s\n", markdownText(step.Decision.Explanation))
		}
		if step.Decision.Error != "" {
			fmt.Fprintf(message, "  - ошибка решения: %s\n", markdownText(step.Decision.Error))
		}
		if len(step.Decision.Skipped) != 0 {
			fmt.Fprintf(message, "  - пропущенные ключи решений: %s\n", markdownText(strings.Join(step.Decision.Skipped, ", ")))
		}
	}
	if routes := decisionRoutesText(step.DecisionRoutes); routes != "" {
		fmt.Fprintf(message, "  - маршруты: %s\n", markdownText(routes))
	}
}

// Summary строит короткий блок для редкой публикации в чат. Состояния, которые
// нельзя честно назвать готовыми, работающими или ожидающими зависимости, не
// скрываются: ошибки, отмена, неизвестность и ожидание подтверждения получают
// отдельный счётчик «требуют внимания». Ссылок на PNG и отдельных кубиков здесь
// намеренно нет — подробности пользователь открывает локально через VS Code.
func Summary(status coordinator.Status, runDir string, artifactErr error) string {
	statistics := countStates(status)
	var message strings.Builder
	fmt.Fprintf(&message, "Всего: %d, готово: %d, работает: %d, ожидают: %d",
		statistics.total, statistics.ready, statistics.running, statistics.waiting)
	if statistics.attention != 0 {
		fmt.Fprintf(&message, ", требуют внимания: %d", statistics.attention)
	}
	message.WriteString(".\n")
	if len(status.WaitingForCapacity) != 0 {
		fmt.Fprintf(&message, "Свободный слот общего лимита ждут: %d.\n", len(status.WaitingForCapacity))
	}
	if isAgentStatus(status) && status.Terminal {
		fmt.Fprintf(&message, "Run %s завершён: %s.\n", markdownText(status.RunID), markdownText(string(status.RunState)))
		if status.StopReason != "" {
			fmt.Fprintf(&message, "Причина: %s\n", markdownText(status.StopReason))
		}
	} else if status.Complete {
		fmt.Fprintf(&message, "Run %s успешно завершён.\n", markdownText(status.RunID))
	}
	fmt.Fprintf(&message, "[Открыть статус в VS Code](%s)\n", vscodeFolderURL(runDir))
	if artifactErr != nil {
		fmt.Fprintf(&message, "Локальные файлы статуса обновлены не полностью: %s\n", safeDiagnostic(artifactErr.Error()))
	}
	message.WriteByte('\n')
	return message.String()
}

// stringSet превращает transient-список причин ожидания в быстрый lookup для
// подробного отчёта. Состояние самого шага остаётся Pending: общий лимит — не
// новое сохранённое состояние планировщика, а причина ожидания текущего процесса.
func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

type stateStatistics struct {
	total, ready, running, waiting, attention int
}

// countStates распределяет каждый шаг ровно в одну пользовательскую категорию.
// Неизвестное будущее состояние относится к требующим внимания, чтобы сумма
// счётчиков оставалась равна total и новая проблема не выглядела как ожидание.
func countStates(status coordinator.Status) stateStatistics {
	statistics := stateStatistics{total: len(status.Steps)}
	for _, step := range status.Steps {
		switch step.State {
		case scheduler.Succeeded:
			statistics.ready++
		case scheduler.Starting, scheduler.Running:
			statistics.running++
		case scheduler.Pending:
			statistics.waiting++
		default:
			statistics.attention++
		}
	}
	return statistics
}

// PlantUML показывает состояние одновременно подписью и цветом. Неизвестное
// будущее значение сохраняется в подписи дословно и получает нейтральный цвет;
// его нельзя спутать с succeeded. Алиасы step_N не используют внешние ID и тем
// самым не дают содержимому workflow изменить синтаксис связей.
func PlantUML(status coordinator.Status) ([]byte, error) {
	if isAgentStatus(status) {
		return agentPlantUML(status)
	}
	return legacyPlantUML(status)
}

// legacyPlantUML сохраняет прежнее представление v1-v3: узлом остаётся step,
// а рёбрами — статические dependsOn. Это исключает визуальную миграцию старых run.
func legacyPlantUML(status coordinator.Status) ([]byte, error) {
	indices := make(map[string]int, len(status.Steps))
	for index, step := range status.Steps {
		if _, exists := indices[step.ID]; exists {
			return nil, fmt.Errorf("повторный ID кубика %q в статусе", step.ID)
		}
		indices[step.ID] = index
	}

	var source strings.Builder
	writePlantHeader(&source, status.WorkflowID)
	for index, step := range status.Steps {
		fmt.Fprintf(&source, "rectangle \"%s\\n%s\" as step_%d %s\n",
			plantText(step.ID), plantText(string(step.State)), index, colorFor(step.State))
	}
	for index, step := range status.Steps {
		for _, dependency := range step.DependsOn {
			parent, exists := indices[dependency]
			if !exists {
				return nil, fmt.Errorf("кубик %q ссылается на отсутствующую зависимость %q", step.ID, dependency)
			}
			fmt.Fprintf(&source, "step_%d --> step_%d\n", parent, index)
		}
	}
	writePlantLegend(&source)
	source.WriteString("@enduml\n")
	return []byte(source.String()), nil
}

// agentPlantUML строит историю v4 по VisitID. Причинные рёбра ссылаются только
// на реально созданные посещения, поэтому невыбранные маршруты не выглядят как
// запущенные или пропущенные узлы.
func agentPlantUML(status coordinator.Status) ([]byte, error) {
	indices := make(map[string]int, len(status.Steps))
	for index, step := range status.Steps {
		if step.VisitID == "" {
			return nil, fmt.Errorf("посещение step %q не содержит visitId", step.ID)
		}
		if _, exists := indices[step.VisitID]; exists {
			return nil, fmt.Errorf("повторный visitId %q в статусе", step.VisitID)
		}
		indices[step.VisitID] = index
	}

	var source strings.Builder
	writePlantHeader(&source, status.WorkflowID)
	for index, step := range status.Steps {
		fmt.Fprintf(&source, "rectangle \"%s\" as visit_%d %s\n", plantText(agentVisitLabel(step)), index, colorFor(step.State))
	}
	for index, step := range status.Steps {
		for _, sourceVisitID := range step.Trigger.SourceVisitIDs {
			parent, exists := indices[sourceVisitID]
			if !exists {
				return nil, fmt.Errorf("посещение %q ссылается на отсутствующий sourceVisitId %q", step.VisitID, sourceVisitID)
			}
			fmt.Fprintf(&source, "visit_%d --> visit_%d : %s\n", parent, index, plantText(triggerEdgeText(step.Trigger)))
		}
	}
	if status.StopVisitID != "" || status.StopReason != "" {
		source.WriteString("note bottom\n")
		fmt.Fprintf(&source, "Run: %s\nStop visit: %s\nReason: %s\n",
			plantText(string(status.RunState)), plantText(status.StopVisitID), plantText(status.StopReason))
		source.WriteString("end note\n")
	}
	writePlantLegend(&source)
	source.WriteString("@enduml\n")
	return []byte(source.String()), nil
}

// agentVisitLabel собирает полную подпись узла до единственного экранирования.
func agentVisitLabel(step coordinator.StepStatus) string {
	lines := []string{
		fmt.Sprintf("%s#%d", step.ID, step.Visit),
		"visitId: " + step.VisitID,
		fmt.Sprintf("iteration: %d; attempt: %d", step.Iteration, step.Attempt),
		string(step.State),
	}
	if step.MaxVisits != nil {
		limit := fmt.Sprintf("maxVisits: %d", *step.MaxVisits)
		if step.OnLimit != nil {
			limit += "; onLimit: " + string(*step.OnLimit)
		}
		lines = append(lines, limit)
	}
	if step.TechnicalError != "" {
		lines = append(lines, "technicalError: "+step.TechnicalError)
	}
	if step.Decision != nil {
		lines = append(lines, fmt.Sprintf("decision: %s; applied: %t", step.Decision.Key, step.Decision.Applied))
		if step.Decision.Explanation != "" {
			lines = append(lines, "decision explanation: "+step.Decision.Explanation)
		}
		if len(step.Decision.Skipped) != 0 {
			lines = append(lines, "skipped keys: "+strings.Join(step.Decision.Skipped, ", "))
		}
		if step.Decision.Error != "" {
			lines = append(lines, "decision error: "+step.Decision.Error)
		}
	}
	if routes := decisionRoutesText(step.DecisionRoutes); routes != "" {
		lines = append(lines, "routes: "+routes)
	}
	return strings.Join(lines, "\n")
}

// writePlantHeader сохраняет общие настройки схем legacy и v4 побайтно
// одинаковыми, чтобы смена модели узлов не затрагивала оформление.
func writePlantHeader(source *strings.Builder, workflowID string) {
	source.WriteString("@startuml\n")
	source.WriteString("skinparam shadowing false\n")
	source.WriteString("skinparam defaultTextAlignment center\n")
	source.WriteString("left to right direction\n")
	fmt.Fprintf(source, "title Workflow: %s\n", plantText(workflowID))
}

// writePlantLegend перечисляет стабильные цвета обоих форматов статуса.
func writePlantLegend(source *strings.Builder) {
	source.WriteString("legend left\n")
	source.WriteString("|= Цвет |= Состояние |\n")
	for _, state := range []scheduler.State{
		scheduler.Pending, scheduler.Starting, scheduler.Running,
		scheduler.Succeeded, scheduler.Failed, scheduler.Unknown,
	} {
		fmt.Fprintf(source, "|<%s> | %s |\n", colorFor(state), state)
	}
	source.WriteString("endlegend\n")
}

// isAgentStatus отличает v4 по авторитетному RunState. У legacy это поле всегда
// пусто, включая завершённые снимки, поэтому их формат остаётся неизменным.
func isAgentStatus(status coordinator.Status) bool { return status.RunState != "" }

// visitTriggerText описывает durable-причину запуска для Markdown.
func visitTriggerText(step coordinator.StepStatus) string {
	trigger := string(step.Trigger.Kind)
	if step.Trigger.DecisionKey != "" {
		trigger += ":" + step.Trigger.DecisionKey
	}
	if len(step.Trigger.SourceVisitIDs) != 0 {
		trigger += " от " + strings.Join(step.Trigger.SourceVisitIDs, ", ")
	}
	return trigger
}

// triggerEdgeText оставляет на ребре тип перехода и выбранный decision-key.
func triggerEdgeText(trigger runstore.VisitTrigger) string {
	label := string(trigger.Kind)
	if trigger.DecisionKey != "" {
		label += ":" + trigger.DecisionKey
	}
	return label
}

// decisionRoutesText показывает статические выходы как атрибут visit, не узлы.
func decisionRoutesText(routes []coordinator.DecisionRouteStatus) string {
	formatted := make([]string, 0, len(routes))
	for _, route := range routes {
		destination := strings.Join(route.To, ", ")
		if route.Finish != nil {
			destination = "finish:" + string(*route.Finish)
		}
		formatted = append(formatted, route.Key+" → "+destination)
	}
	return strings.Join(formatted, "; ")
}

// colorFor задаёт стабильную легенду известных состояний. Нейтральный fallback
// намеренно совпадает с unknown только цветом: подпись сохраняет новое значение.
func colorFor(state scheduler.State) string {
	switch state {
	case scheduler.Pending:
		return "#F3F4F6"
	case scheduler.Starting:
		return "#FDE68A"
	case scheduler.Running:
		return "#93C5FD"
	case scheduler.Succeeded:
		return "#86EFAC"
	case scheduler.Failed:
		return "#FCA5A5"
	case scheduler.WaitingForApproval:
		return "#FBCFE8"
	case scheduler.Cancelled:
		return "#FDBA74"
	case scheduler.Unknown:
		return "#D1D5DB"
	default:
		return "#D1D5DB"
	}
}

// codexThreadURL кодирует внешний ID как единственный сегмент пути. Это не даёт
// символам из app-server изменить host или добавить параметры deeplink.
func codexThreadURL(threadID string) string {
	return (&url.URL{Scheme: "codex", Host: "threads", Path: "/" + threadID}).String()
}

// vscodeFolderURL строит поддерживаемый VS Code deeplink для абсолютной папки run.
func vscodeFolderURL(path string) string {
	return (&url.URL{Scheme: "vscode", Host: "file", Path: filepath.ToSlash(path)}).String()
}

// markdownText оставляет видимый текст узнаваемым, но заменяет переносы и прочие
// управляющие символы печатной записью. Скобки экранируются, чтобы ID не закрыл
// собственную Markdown-ссылку; HTML-границы кодируются, чтобы model-controlled
// пояснение не добавило тег с локальным либо сетевым ресурсом.
func markdownText(value string) string {
	var result strings.Builder
	for _, character := range value {
		switch character {
		case '\\', '[', ']':
			result.WriteByte('\\')
			result.WriteRune(character)
		case '&':
			result.WriteString("&amp;")
		case '<':
			result.WriteString("&lt;")
		case '>':
			result.WriteString("&gt;")
		case '\n':
			result.WriteString(`\n`)
		case '\r':
			result.WriteString(`\r`)
		default:
			if unicode.IsControl(character) || unicode.In(character, unicode.Zl, unicode.Zp) {
				fmt.Fprintf(&result, `\u%04X`, character)
			} else {
				result.WriteRune(character)
			}
		}
	}
	return result.String()
}

// plantText не позволяет данным workflow попасть в синтаксис, preprocessor или
// Creole/HTML PlantUML. Вся ASCII-пунктуация кодируется numeric entity и при
// рендере выглядит исходным символом, но `%chr`, `<img>`, ссылки и embedded
// diagrams не распознаются как разметка. Переводы строк остаются безопасным
// `\n` внутри одной директивы; Unicode Zl/Zp печатаются как видимый код.
func plantText(value string) string {
	var result strings.Builder
	for _, character := range value {
		switch {
		case character == '\n' || character == '\r':
			result.WriteString(`\n`)
		case unicode.IsControl(character) || unicode.In(character, unicode.Zl, unicode.Zp):
			fmt.Fprintf(&result, `\u%04X`, character)
		case character <= unicode.MaxASCII && character != ' ' &&
			(character < '0' || character > '9') &&
			(character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z'):
			fmt.Fprintf(&result, "&#%d;", character)
		default:
			result.WriteRune(character)
		}
	}
	return result.String()
}

// safeDiagnostic применяет те же ограничения к stderr внешнего renderer.
func safeDiagnostic(value string) string {
	return markdownText(strings.TrimSpace(value))
}

// writeAtomic полностью записывает и синхронизирует временный файл до rename.
// Права 0600 совпадают с остальными данными run и не раскрывают постановку задачи.
func writeAtomic(path string, data []byte) (err error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, temporary.Close())
		}
		removeErr := os.Remove(temporaryPath)
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		closed = true
		err = closeErr
	} else {
		closed = true
	}
	if err == nil {
		err = os.Rename(temporaryPath, path)
	}
	return err
}

// removeArtifact удаляет только точный служебный файл. Отсутствие уже означает
// нужное состояние и не превращается в дополнительную ошибку визуализации.
func removeArtifact(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("удалить устаревший артефакт %q: %w", path, err)
	}
	return nil
}
