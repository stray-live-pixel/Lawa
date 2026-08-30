// Package statusreport превращает один снимок координатора в сообщение для
// чата-инициатора и локальные PlantUML-артефакты. Пакет ничего не читает из run:
// вызывающий код передаёт уже согласованные состояния и зависимости, поэтому
// текст и схема одного обновления не могут относиться к разным версиям meta.json.
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
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

const (
	// SourceFilename и ImageFilename — стабильные имена последнего снимка в run.
	// Временные файлы публикуются через rename, поэтому пользователь не увидит
	// частично записанный source или PNG.
	SourceFilename = "workflow-status.puml"
	ImageFilename  = "workflow-status.png"
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

// Artifacts перечисляет только успешно опубликованные файлы. SourcePath может
// быть заполнен без ImagePath, если source сохранён, а локальный renderer сломан.
type Artifacts struct {
	SourcePath string
	ImagePath  string
}

// WriteArtifacts сохраняет source и PNG в папке конкретного run. При ошибке
// рендера актуальный source остаётся доступен для диагностики, а прежний PNG
// удаляется: основной агент не должен случайно приложить старую картинку к новому
// текстовому снимку. Ошибка не меняет meta.json, workflow.json или память кубиков.
func WriteArtifacts(ctx context.Context, runDir string, status coordinator.Status, renderer Renderer) (Artifacts, error) {
	if !filepath.IsAbs(runDir) {
		return Artifacts{}, fmt.Errorf("папка run должна быть абсолютной: %q", runDir)
	}
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

// Message строит один готовый Markdown-блок для исходного чата. Каждый кубик
// появляется ровно один раз и в порядке meta.json. Пустой codexThreadId никогда
// не подменяется ссылкой на инициатора или другой чат.
func Message(status coordinator.Status, runDir string, artifacts Artifacts, visualizationErr error) string {
	var message strings.Builder
	fmt.Fprintf(&message, "Статус workflow \"%s\":\n", markdownText(status.WorkflowID))
	for _, step := range status.Steps {
		label := markdownText(step.ID)
		if step.CodexThreadID != "" {
			label = fmt.Sprintf("[%s](%s)", label, codexThreadURL(step.CodexThreadID))
		}
		fmt.Fprintf(&message, "- %s — %s", label, markdownText(string(step.State)))
		if step.CodexThreadID == "" {
			message.WriteString(" (чат ещё не создан)")
		}
		message.WriteByte('\n')
	}
	if status.Complete {
		message.WriteByte('\n')
		fmt.Fprintf(&message, "Run %s успешно завершён.\n", markdownText(status.RunID))
	}
	fmt.Fprintf(&message, "\n[Открыть папку run в VS Code](%s)\n", vscodeFolderURL(runDir))
	if visualizationErr == nil && artifacts.SourcePath != "" && artifacts.ImagePath != "" {
		fmt.Fprintf(&message, "PlantUML source: %s\nPlantUML image: %s\n",
			markdownText(artifacts.SourcePath), markdownText(artifacts.ImagePath))
	} else {
		message.WriteString("Схема PlantUML не обновлена")
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

// PlantUML показывает состояние одновременно подписью и цветом. Неизвестное
// будущее значение сохраняется в подписи дословно и получает нейтральный цвет;
// его нельзя спутать с succeeded. Алиасы step_N не используют внешние ID и тем
// самым не дают содержимому workflow изменить синтаксис связей.
func PlantUML(status coordinator.Status) ([]byte, error) {
	indices := make(map[string]int, len(status.Steps))
	for index, step := range status.Steps {
		if _, exists := indices[step.ID]; exists {
			return nil, fmt.Errorf("повторный ID кубика %q в статусе", step.ID)
		}
		indices[step.ID] = index
	}

	var source strings.Builder
	source.WriteString("@startuml\n")
	source.WriteString("skinparam shadowing false\n")
	source.WriteString("skinparam defaultTextAlignment center\n")
	source.WriteString("left to right direction\n")
	fmt.Fprintf(&source, "title Workflow: %s\n", plantText(status.WorkflowID))
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
	source.WriteString("legend left\n")
	source.WriteString("|= Цвет |= Состояние |\n")
	for _, state := range []scheduler.State{
		scheduler.Pending, scheduler.Starting, scheduler.Running,
		scheduler.Succeeded, scheduler.Failed, scheduler.Unknown,
	} {
		fmt.Fprintf(&source, "|<%s> | %s |\n", colorFor(state), state)
	}
	source.WriteString("endlegend\n")
	source.WriteString("@enduml\n")
	return []byte(source.String()), nil
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
// собственную Markdown-ссылку и не подменил её адрес.
func markdownText(value string) string {
	var result strings.Builder
	for _, character := range value {
		switch character {
		case '\\', '[', ']':
			result.WriteByte('\\')
			result.WriteRune(character)
		case '\n':
			result.WriteString(`\n`)
		case '\r':
			result.WriteString(`\r`)
		default:
			if unicode.IsControl(character) {
				fmt.Fprintf(&result, `\u%04X`, character)
			} else {
				result.WriteRune(character)
			}
		}
	}
	return result.String()
}

// plantText не позволяет данным workflow завершить строку или кавычки PlantUML.
// Переводы строк сохраняются как видимый `\n` внутри подписи узла.
func plantText(value string) string {
	var result strings.Builder
	for _, character := range value {
		switch character {
		case '\\':
			result.WriteString(`\\`)
		case '"':
			result.WriteString(`\"`)
		case '\n', '\r':
			result.WriteString(`\n`)
		default:
			if unicode.IsControl(character) {
				fmt.Fprintf(&result, `\u%04X`, character)
			} else {
				result.WriteRune(character)
			}
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
