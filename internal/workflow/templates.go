package workflow

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// readTemplates читает реестр только на границе исходного workflow. Значение —
// строка либо строгий объект {file}; файлы расположены под templates/ рядом с JSON.
// Файловые полномочия и проверку обычного файла сохраняет переданный readFile,
// включая защиту дочерних запусков от выхода через симлинки за разрешённые корни.
func readTemplates(raw jsontext.Value, sourcePath string, readFile func(string) ([]byte, error)) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("templates: нужен объект именованных шаблонов")
	}
	var entries map[string]jsontext.Value
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	result := make(map[string]string, len(entries))
	// Сортировка делает первую ошибку воспроизводимой при нескольких дефектах.
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" || strings.HasPrefix(name, "lawa.") || strings.IndexFunc(name, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-'
		}) >= 0 {
			return nil, fmt.Errorf("templates: недопустимое имя %q; нужны буквы, цифры, _ или -", name)
		}
		value := bytes.TrimSpace(entries[name])
		var content string
		if len(value) > 0 && value[0] == '"' {
			if err := json.Unmarshal(value, &content); err != nil {
				return nil, fmt.Errorf("templates.%s: %w", name, err)
			}
		} else {
			var ref struct {
				File string `json:"file"`
			}
			if err := json.Unmarshal(value, &ref, json.RejectUnknownMembers(true)); err != nil {
				return nil, fmt.Errorf("templates.%s: %w", name, err)
			}
			path := filepath.Clean(ref.File)
			if !filepath.IsLocal(ref.File) || !strings.HasPrefix(path, "templates"+string(filepath.Separator)) || !strings.EqualFold(filepath.Ext(path), ".md") {
				return nil, fmt.Errorf("templates.%s.file: нужен относительный путь templates/*.md внутри папки workflow", name)
			}
			data, err := readFile(filepath.Join(filepath.Dir(sourcePath), path))
			if err != nil {
				return nil, fmt.Errorf("templates.%s.file %q: %w", name, ref.File, err)
			}
			content = string(data)
		}
		if !utf8.ValidString(content) || strings.TrimSpace(content) == "" {
			return nil, fmt.Errorf("templates.%s: нужен непустой текст UTF-8", name)
		}
		result[name] = content
	}
	return result, nil
}

// templateContext содержит только статические значения проверенного workflow.
// Модель шага наследует workflow; отсутствующие настройки Codex неизвестны здесь
// и представлены пустой строкой. ID запуска и номер визита не входят в контекст:
// снимок создаётся до run и переиспользуется при resume и повторах серии.
func templateContext(w Workflow, step Step) map[string]string {
	value := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	model := step.Model
	if model == nil {
		model = w.Model
	}
	speed := ""
	if step.Speed != nil {
		speed = string(*step.Speed)
	}
	return map[string]string{
		"lawa.workflow.id":      w.ID,
		"lawa.workflow.version": strconv.Itoa(w.EffectiveVersion()),
		"lawa.workflow.model":   value(w.Model),
		"lawa.step.id":          step.ID,
		"lawa.step.type":        step.Type,
		"lawa.step.model":       value(model),
		"lawa.step.effort":      value(step.Effort),
		"lawa.step.speed":       speed,
	}
}

// expandPrompt раскрывает вложенные шаблоны с отдельным кешем на кубик, поскольку
// один шаблон получает разные lawa.step.*. Активный стек обнаруживает циклы;
// лимиты глубины и размера защищают от экспоненциального роста валидного DAG.
// Сканируется только исходный текст каждого уровня: значения контекста и
// экранированные фрагменты не становятся новыми инструкциями шаблонизатора.
func expandPrompt(prompt string, templates, context map[string]string) (string, error) {
	const maxDepth = 64
	const maxBytes = 1 << 20
	// Глубина входит в ключ: результат, полученный у корня, не должен позволить
	// обойти лимит при вставке того же шаблона в длинную цепочку.
	type cacheKey struct {
		name  string
		depth int
	}
	cache := make(map[cacheKey]string)
	active := make(map[string]bool)
	var expand func(string, int) (string, error)
	var resolve func(string, int) (string, error)
	resolve = func(name string, depth int) (string, error) {
		if value, ok := context[name]; ok {
			return value, nil
		}
		key := cacheKey{name, depth}
		if value, ok := cache[key]; ok {
			return value, nil
		}
		source, ok := templates[name]
		if !ok {
			return "", fmt.Errorf("неизвестный шаблон или параметр %q", name)
		}
		if active[name] {
			return "", fmt.Errorf("цикл шаблонов через %q", name)
		}
		if depth >= maxDepth {
			return "", fmt.Errorf("шаблон %q: глубина превышает %d", name, maxDepth)
		}
		active[name] = true
		value, err := expand(source, depth+1)
		delete(active, name)
		if err != nil {
			return "", fmt.Errorf("шаблон %q: %w", name, err)
		}
		cache[key] = value
		return value, nil
	}
	expand = func(source string, depth int) (string, error) {
		// Полностью обычный prompt сохраняется без новых ограничений размера.
		if !strings.Contains(source, "{{") {
			return source, nil
		}
		var out strings.Builder
		for len(source) > 0 {
			start := strings.Index(source, "{{")
			if start < 0 {
				start = len(source)
			}
			prefix, rest := source[:start], source[start:]
			if rest == "" {
				if out.Len()+len(prefix) > maxBytes {
					return "", fmt.Errorf("раскрытый текст превышает %d байт", maxBytes)
				}
				out.WriteString(prefix)
				break
			}
			end := strings.Index(rest[2:], "}}")
			if end < 0 {
				return "", fmt.Errorf("незакрытая подстановка {{")
			}
			end += 4
			var replacement string
			if strings.HasSuffix(prefix, "\\") {
				prefix = prefix[:len(prefix)-1]
				replacement = rest[:end]
			} else {
				var err error
				replacement, err = resolve(strings.TrimSpace(rest[2:end-2]), depth)
				if err != nil {
					return "", err
				}
			}
			if out.Len()+len(prefix)+len(replacement) > maxBytes {
				return "", fmt.Errorf("раскрытый текст превышает %d байт", maxBytes)
			}
			out.WriteString(prefix)
			out.WriteString(replacement)
			source = rest[end:]
		}
		return out.String(), nil
	}
	// Проверяем даже неиспользуемые шаблоны, чтобы опечатка не ждала нового кубика.
	names := make([]string, 0, len(templates))
	for name := range templates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := resolve(name, 0); err != nil {
			return "", err
		}
	}
	return expand(prompt, 0)
}
