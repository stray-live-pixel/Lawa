package reviewruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

// stageSession существует лишь внутри одной попытки. completed выставляется
// только после валидации данных, поэтому свободный ответ не подменяет контракт.
type stageSession struct {
	engine        *Engine
	id            string
	stage         reviewstore.StageID
	number        int
	scratch       string
	completed     bool
	wrote         bool
	output        string
	eventNumber   int
	childSeen     bool
	eventPrefix   string
	actorThreadID string
}

// Входы инструментов отделены от Review: агент не меняет настройки, статусы и даты.
type contextInput struct {
	Title, SourceRevision, ChangeURL string
	Context                          reviewstore.Context
}
type findingsInput struct {
	Findings        []reviewstore.Finding
	EvidenceFiles   []reviewstore.File
	EvidenceDesigns []reviewstore.Design
}
type artifactInput struct{ Path, Text, Base64, SourcePath string }
type skillInput struct{ Path string }
type publicationInput struct{ Publication reviewstore.Publication }
type finishInput struct{ Summary string }

// schema строит описание только из публичных типов контракта, включая вложенные
// массивы. Это одновременно документация инструментов и защита от расхождения API.
func schema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		return schema(t.Elem())
	}
	if t == reflect.TypeOf(time.Time{}) {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.Struct:
		p := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.IsExported() {
				p[f.Name] = schema(f.Type)
			}
		}
		return map[string]any{"type": "object", "properties": p, "additionalProperties": false}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schema(t.Elem())}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "string"}
	}
}

// tools открывает только операции своего этапа; исходный контекст недоступен для
// перезаписи проверке и презентации. Артефакт создаётся до ссылки на него.
func (s *stageSession) tools() []codex.DynamicTool {
	t := []codex.DynamicTool{}
	add := func(name, description string, v any) {
		b, _ := json.Marshal(schema(reflect.TypeOf(v)))
		t = append(t, codex.DynamicTool{Name: name, Description: description, InputSchema: b})
	}
	add("review_artifact", "Сохранить неизменяемый артефакт. Path начинается artifacts/. Укажите ровно одно: Text, Base64 либо абсолютный SourcePath внутри проекта или scratch. Возвращает путь.", artifactInput{})
	add("review_read_skill", "Прочитать настоящий SKILL.md и сохранить точный использованный текст. Вызов регистрирует чтение, а не выдуманное выполнение скилла.", skillInput{})
	switch s.stage {
	case reviewstore.ContextStage:
		add("review_context", "Постепенно заменить собранный контекст. Ссылки провайдер-нейтральны; задачи упорядочены по приоритету. Не устанавливайте FrozenAt. Сохраняйте файлы инструментом review_artifact заранее.", contextInput{})
	case reviewstore.ReviewStage:
		add("review_delegate", "Запустить субагента для конкретной части проверки и дождаться ответа. Сохраняет точный prompt, модель, effort, статус и фактические токены. По умолчанию параметры этапа; Model/Effort можно задать явно. Предпочтительный способ делегирования вместо native spawn.", delegateInput{})
		add("review_findings", "Сохранить полный текущий список доказуемых замечаний и дополнительных доказательств. P0/P1 блокируют; P2/P3 рекомендации. Не удаляйте уже собранные доказательства без причины.", findingsInput{})
	case reviewstore.PresentationStage:
		add("review_presentation", "Сохранить обзор изменений и маршруты доказательства каждого замечания. Один шаг — одна мысль; ссылки только на сохранённые материалы. Markdown и исправляющие промпты приложение сформирует из этих же данных.", reviewstore.Presentation{})
		add("review_publication", "Зафиксировать planned перед внешней публикацией и published после подтверждения URL/ID. Не повторять неоднозначную публикацию: сначала найти её в PR по маркеру. Отказ записать failed.", publicationInput{})
	}
	add("review_finish", "Завершить этап после сохранения всех данных. Проверяет контракт; при ошибке исправьте материалы и повторите. Не заменяет обычное завершение turn.", finishInput{})
	return t
}

// decode запрещает случайные поля, которые иначе могли бы тихо пропасть.
func decode(raw []byte, v any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// call проверяет этап и сохраняет только разрешённую часть сущности.
func (s *stageSession) call(ctx context.Context, c codex.DynamicToolCall) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.completed {
		return "", errors.New("этап уже завершён")
	}
	switch c.Tool {
	case "review_delegate":
		if s.stage != reviewstore.ReviewStage {
			return "", errors.New("делегирование доступно на этапе проверки")
		}
		var in delegateInput
		if err := decode(c.Arguments, &in); err != nil {
			return "", err
		}
		return s.delegate(ctx, c.CallID, in)
	case "review_artifact":
		var in artifactInput
		if err := decode(c.Arguments, &in); err != nil {
			return "", err
		}
		return s.artifact(in)
	case "review_read_skill":
		var in skillInput
		if err := decode(c.Arguments, &in); err != nil {
			return "", err
		}
		return s.skill(in.Path)
	case "review_context":
		if s.stage != reviewstore.ContextStage {
			return "", errors.New("контекст уже зафиксирован")
		}
		var in contextInput
		if err := decode(c.Arguments, &in); err != nil {
			return "", err
		}
		if in.Context.FrozenAt != nil {
			return "", errors.New("FrozenAt устанавливает runtime")
		}
		_, err := s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
			r.Title = in.Title
			r.SourceRevision = in.SourceRevision
			r.ChangeURL = in.ChangeURL
			seen := map[string]bool{}
			for _, command := range in.Context.Commands {
				seen[command.ID] = true
			}
			for _, command := range r.Context.Commands {
				if strings.HasPrefix(command.ID, "observed-") && !seen[command.ID] {
					in.Context.Commands = append(in.Context.Commands, command)
				}
			}
			r.Context = in.Context
			return nil
		})
		if err == nil {
			s.wrote = true
		}
		return "Контекст сохранён", err
	case "review_findings":
		if s.stage != reviewstore.ReviewStage {
			return "", errors.New("замечания сохраняет проверяющий агент")
		}
		var in findingsInput
		if err := decode(c.Arguments, &in); err != nil {
			return "", err
		}
		for _, f := range in.Findings {
			if err := validFinding(f); err != nil {
				return "", err
			}
		}
		_, err := s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
			r.Findings = in.Findings
			r.EvidenceFiles = in.EvidenceFiles
			r.EvidenceDesigns = in.EvidenceDesigns
			r.Verdict = "approve"
			for _, f := range r.Findings {
				if f.Priority == "P0" || f.Priority == "P1" {
					r.Verdict = "request_changes"
				}
			}
			return nil
		})
		if err == nil {
			s.wrote = true
		}
		return "Замечания сохранены", err
	case "review_presentation":
		if s.stage != reviewstore.PresentationStage {
			return "", errors.New("не этап презентации")
		}
		var in reviewstore.Presentation
		if err := decode(c.Arguments, &in); err != nil {
			return "", err
		}
		result, err := s.presentation(in)
		if err == nil {
			s.wrote = true
		}
		return result, err
	case "review_publication":
		if s.stage != reviewstore.PresentationStage {
			return "", errors.New("не этап публикации")
		}
		var in publicationInput
		if err := decode(c.Arguments, &in); err != nil {
			return "", err
		}
		result, err := s.publication(in.Publication)
		if err == nil {
			s.wrote = true
		}
		return result, err
	case "review_finish":
		if !s.wrote {
			return "", errors.New("сначала сохраните результат текущей попытки")
		}
		if err := s.validate(); err != nil {
			return "", err
		}
		s.completed = true
		return "Контракт этапа проверен. Завершите ответ.", nil
	default:
		return "", errors.New("неизвестный инструмент review")
	}
}

// artifact ограничивает импорт локальных файлов проектом и временной папкой
// попытки; symlink не расширяет разрешённую область. Binary передаётся base64.
func (s *stageSession) artifact(in artifactInput) (string, error) {
	var data []byte
	count := 0
	var err error
	if in.Text != "" {
		data = []byte(in.Text)
		count++
	}
	if in.Base64 != "" {
		data, err = base64.StdEncoding.DecodeString(in.Base64)
		if err != nil {
			return "", err
		}
		count++
	}
	if in.SourcePath != "" {
		count++
		r, err := s.engine.Store.Load(s.id)
		if err != nil {
			return "", err
		}
		path, err := filepath.EvalSymlinks(in.SourcePath)
		if err != nil {
			return "", err
		}
		if !within(r.CWD, path) && !within(s.scratch, path) {
			return "", errors.New("SourcePath должен находиться в проекте или scratch")
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Size() > 20<<20 {
			return "", errors.New("ожидается обычный файл до 20 MiB")
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return "", err
		}
	}
	if count != 1 {
		return "", errors.New("нужен ровно один непустой источник Text, Base64 или SourcePath")
	}
	if len(data) > 20<<20 {
		return "", errors.New("артефакт превышает 20 MiB")
	}
	return in.Path, s.engine.Store.SaveArtifact(s.id, in.Path, data)
}

// within сравнивает пути компонентами, а не строковым префиксом.
func within(root, path string) bool {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && filepath.IsAbs(path)
}

// skill открывает явный файл навыка: это наблюдаемое чтение. Текст сохраняется
// до возврата модели, поэтому viewer показывает именно использованную версию.
func (s *stageSession) skill(path string) (string, error) {
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) || filepath.Base(path) != "SKILL.md" {
		return "", errors.New("нужен абсолютный путь к SKILL.md")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", errors.New("ожидается SKILL.md до 1 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	r, err := s.engine.Store.Load(s.id)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("skill-%s-%d-%d", s.stage, s.number, len(r.Activities))
	artifact := "artifacts/skills/" + key + ".md"
	if err = s.engine.Store.SaveArtifact(s.id, artifact, data); err != nil {
		return "", err
	}
	_, err = s.engine.Store.Update(s.id, func(r *reviewstore.Review) error {
		r.Activities = append(r.Activities, reviewstore.Activity{Stage: s.stage, Attempt: s.number, ID: key, Kind: "skill", Title: filepath.Base(filepath.Dir(path)), State: "read", DocumentPath: artifact, ThreadID: s.actorThreadID, At: time.Now().UTC()})
		return nil
	})
	return string(data), err
}

// validFinding требует полезное доказательство, но не заставляет выдумывать
// привязку к коду, если её действительно нет.
func validFinding(f reviewstore.Finding) error {
	if f.ID == "" || f.Title == "" || f.Explanation == "" || f.Reproduction == "" || f.Consequence == "" {
		return errors.New("замечание требует ID, заголовок, обоснование, воспроизведение и последствия")
	}
	if f.Priority != "P0" && f.Priority != "P1" && f.Priority != "P2" && f.Priority != "P3" {
		return errors.New("приоритет должен быть P0–P3")
	}
	for _, a := range f.Anchors {
		if a.FileID == "" && a.TaskID == "" && a.DesignID == "" && strings.TrimSpace(a.NoAnchorReason) == "" {
			return errors.New("пустая привязка замечания")
		}
	}
	if len(f.Anchors) == 0 {
		return errors.New("нужны доказательства или Anchor.NoAnchorReason")
	}
	return nil
}
