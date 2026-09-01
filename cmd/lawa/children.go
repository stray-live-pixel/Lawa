package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/capacity"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// childInputSchema — единый контракт одного дочернего запуска. Ровно одна форма
// задачи исключает неоднозначность, а additionalProperties не позволяет опечатке
// тихо превратиться в значение по умолчанию.
var childInputSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "workflow":{"type":"string"},
    "cwd":{"type":"string"},
    "task":{"type":"string"},
    "taskFile":{"type":"string"},
    "parentRun":{"type":"string"}
  },
  "required":["workflow","cwd","parentRun"],
  "oneOf":[{"required":["task"]},{"required":["taskFile"]}]
}`)

var nativeChildTools = []codex.DynamicTool{
	{
		Name: "run_child", Description: "Надёжно зарегистрировать и запустить один дочерний workflow Lawa; возвращает runId.",
		InputSchema: childInputSchema,
	},
	{
		Name: "run_children", Description: "Надёжно зарегистрировать и параллельно запустить несколько дочерних workflow Lawa; возвращает runIds.",
		InputSchema: json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{"children":{"type":"array","minItems":1,"maxItems":32,"items":` + string(childInputSchema) + `}},
  "required":["children"]
}`),
	},
}

type childRequest struct {
	Workflow  string `json:"workflow"`
	CWD       string `json:"cwd"`
	Task      string `json:"task"`
	TaskFile  string `json:"taskFile"`
	ParentRun string `json:"parentRun"`
}

type resolvedChild struct {
	input runstore.Input
}

type childCall struct {
	done   chan struct{}
	runIDs []string
	err    error
}

// childRunManager владеет всем деревом, созданным одним верхнеуровневым run или
// resume. Дочерние координаторы работают в этом же процессе и используют тот же
// root-level capacity.Pool. Поэтому отдельный daemon и наследование shell-прав
// не нужны, а завершение CLI ждёт всех зарегистрированных потомков.
type childRunManager struct {
	ctx              context.Context
	root, executable string
	pool             *capacity.Pool
	stderr           io.Writer
	deps             dependencies
	mu               sync.Mutex
	calls            map[string]*childCall
	started          map[string]bool
	errors           []error
	wg               sync.WaitGroup
}

func newChildRunManager(ctx context.Context, root, executable string, pool *capacity.Pool, stderr io.Writer, deps dependencies) *childRunManager {
	return &childRunManager{
		ctx: ctx, root: root, executable: executable, pool: pool, stderr: stderr, deps: deps,
		calls: map[string]*childCall{}, started: map[string]bool{},
	}
}

// configure объявляет инструменты каждому turn, включая продолженный. Обработчик
// привязан к сохранённому snapshot: переданный моделью parentRun нельзя подменить
// на соседний run, к которому этот кубик не относится.
func (m *childRunManager) configure(snapshot runstore.Snapshot, command *codex.Command) {
	command.DynamicTools = append([]codex.DynamicTool(nil), nativeChildTools...)
	command.CallDynamicTool = func(ctx context.Context, call codex.DynamicToolCall) (string, error) {
		return m.handle(ctx, snapshot, call)
	}
}

func (m *childRunManager) handle(ctx context.Context, parent runstore.Snapshot, call codex.DynamicToolCall) (string, error) {
	requests, err := decodeChildRequests(call.Tool, call.Arguments)
	if err != nil {
		return "", err
	}
	resolved := make([]resolvedChild, 0, len(requests))
	for index, request := range requests {
		child, resolveErr := m.resolve(parent, request)
		if resolveErr != nil {
			return "", fmt.Errorf("дочерний запуск %d: %w", index+1, resolveErr)
		}
		resolved = append(resolved, child)
	}
	key, err := childFingerprint(resolved)
	if err != nil {
		return "", err
	}
	runIDs, err := m.launchOnce(ctx, key, resolved)
	if err != nil {
		return "", err
	}
	var result []byte
	if call.Tool == "run_child" {
		result, err = json.Marshal(map[string]string{"runId": runIDs[0]})
	} else {
		result, err = json.Marshal(map[string][]string{"runIds": runIDs})
	}
	return string(result), err
}

func decodeChildRequests(tool string, arguments json.RawMessage) ([]childRequest, error) {
	decode := func(raw []byte, target any) error {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return err
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return errors.New("после JSON-объекта есть лишние данные")
		}
		return nil
	}
	switch tool {
	case "run_child":
		var request childRequest
		if err := decode(arguments, &request); err != nil {
			return nil, fmt.Errorf("прочитать run_child: %w", err)
		}
		return []childRequest{request}, nil
	case "run_children":
		var batch struct {
			Children []childRequest `json:"children"`
		}
		if err := decode(arguments, &batch); err != nil {
			return nil, fmt.Errorf("прочитать run_children: %w", err)
		}
		if len(batch.Children) == 0 || len(batch.Children) > 32 {
			return nil, errors.New("run_children требует от 1 до 32 дочерних запусков")
		}
		return batch.Children, nil
	default:
		return nil, fmt.Errorf("неподдерживаемый инструмент %q", tool)
	}
}

// resolve читает файлы самим процессом Lawa, но не расширяет доступ агента:
// child cwd обязан оставаться внутри текущего workspace, а workflow/task-file —
// внутри workspace либо доступной только для чтения папки родительского run.
func (m *childRunManager) resolve(parent runstore.Snapshot, request childRequest) (resolvedChild, error) {
	if request.ParentRun != parent.Meta.RunID {
		return resolvedChild{}, fmt.Errorf("parentRun должен быть текущим runId %q", parent.Meta.RunID)
	}
	if !utf8.ValidString(request.Workflow+request.CWD+request.Task+request.TaskFile) ||
		strings.TrimSpace(request.Workflow) == "" || strings.TrimSpace(request.CWD) == "" {
		return resolvedChild{}, errors.New("workflow, cwd и текстовые поля должны быть корректным UTF-8")
	}
	if (strings.TrimSpace(request.Task) == "") == (strings.TrimSpace(request.TaskFile) == "") {
		return resolvedChild{}, errors.New("нужно указать ровно одно из task и taskFile")
	}
	workspace, err := filepath.EvalSymlinks(parent.Meta.CWD)
	if err != nil {
		return resolvedChild{}, fmt.Errorf("проверить workspace родителя: %w", err)
	}
	runDir, err := filepath.EvalSymlinks(filepath.Join(m.root, parent.Meta.RunID))
	if err != nil {
		return resolvedChild{}, fmt.Errorf("проверить папку родительского run: %w", err)
	}
	cwd, err := resolveExistingPath(workspace, request.CWD)
	if err != nil {
		return resolvedChild{}, fmt.Errorf("проверить cwd: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("не является папкой")
		}
		return resolvedChild{}, fmt.Errorf("проверить cwd: %w", err)
	}
	if !inside(workspace, cwd) {
		return resolvedChild{}, errors.New("cwd дочернего workflow должен находиться внутри workspace родителя")
	}
	workflowPath, err := resolveReadableFile(workspace, runDir, request.Workflow)
	if err != nil {
		return resolvedChild{}, fmt.Errorf("прочитать workflow: %w", err)
	}
	workflowJSON, err := os.ReadFile(workflowPath)
	if err != nil {
		return resolvedChild{}, fmt.Errorf("прочитать workflow: %w", err)
	}
	if _, err = workflow.Decode(bytes.NewReader(workflowJSON)); err != nil {
		return resolvedChild{}, fmt.Errorf("проверить workflow: %w", err)
	}
	task := request.Task
	if request.TaskFile != "" {
		taskPath, pathErr := resolveReadableFile(workspace, runDir, request.TaskFile)
		if pathErr != nil {
			return resolvedChild{}, fmt.Errorf("прочитать taskFile: %w", pathErr)
		}
		data, readErr := os.ReadFile(taskPath)
		if readErr != nil {
			return resolvedChild{}, fmt.Errorf("прочитать taskFile: %w", readErr)
		}
		task = string(data)
	}
	if !utf8.ValidString(task) || strings.TrimSpace(task) == "" {
		return resolvedChild{}, errors.New("task должен быть непустым текстом UTF-8")
	}
	return resolvedChild{input: runstore.Input{
		WorkflowJSON: workflowJSON, Task: task, CWD: cwd, ParentRunID: parent.Meta.RunID,
	}}, nil
}

func resolveExistingPath(base, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.EvalSymlinks(filepath.Clean(path))
}

func resolveReadableFile(workspace, runDir, path string) (string, error) {
	resolved, err := resolveExistingPath(workspace, path)
	if err != nil {
		return "", err
	}
	if !inside(workspace, resolved) && !inside(runDir, resolved) {
		return "", errors.New("файл должен находиться внутри workspace или родительского run")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("путь не является обычным файлом")
	}
	return resolved, nil
}

func inside(base, target string) bool {
	relative, err := filepath.Rel(base, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func childFingerprint(children []resolvedChild) (string, error) {
	inputs := make([]runstore.Input, 0, len(children))
	for _, child := range children {
		inputs = append(inputs, child.input)
	}
	data, err := json.Marshal(inputs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// launchOnce сериализует одинаковые запросы и повторно использует уже
// опубликованный child из обычного runstore. Create возвращается только после
// fsync, поэтому runId отдаётся агенту не раньше надёжной регистрации.
func (m *childRunManager) launchOnce(ctx context.Context, key string, children []resolvedChild) ([]string, error) {
	m.mu.Lock()
	if existing := m.calls[key]; existing != nil {
		m.mu.Unlock()
		select {
		case <-existing.done:
			return append([]string(nil), existing.runIDs...), existing.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &childCall{done: make(chan struct{})}
	m.calls[key] = call
	m.mu.Unlock()

	for _, child := range children {
		snapshot, found, err := runstore.FindMatchingChild(m.root, child.input)
		if err == nil && !found {
			snapshot, err = runstore.Create(m.root, child.input)
		}
		if err != nil {
			call.err = fmt.Errorf("зарегистрировать дочерний run после %v: %w", call.runIDs, err)
			break
		}
		// Результат сохраняет позиционное соответствие batch: одинаковые входы
		// получают одинаковый ID дважды, но start ниже всё равно идемпотентен.
		call.runIDs = append(call.runIDs, snapshot.Meta.RunID)
		m.start(snapshot.Meta.RunID, found)
	}
	m.mu.Lock()
	close(call.done)
	m.mu.Unlock()
	return append([]string(nil), call.runIDs...), call.err
}

func (m *childRunManager) start(runID string, resume bool) {
	m.mu.Lock()
	if m.started[runID] {
		m.mu.Unlock()
		return
	}
	m.started[runID] = true
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		_, err := coordinateRunWithOutcome(m.ctx, m.root, runID, m.executable, m.pool, io.Discard, m.stderr, m.deps, resume, true, m)
		if err != nil {
			m.mu.Lock()
			m.errors = append(m.errors, fmt.Errorf("дочерний run %s: %w", runID, err))
			m.mu.Unlock()
		}
	}()
}

// wait вызывается после завершения корневого координатора. Пока дочерний run
// активен, счётчик больше нуля, поэтому он может безопасно добавить в то же дерево
// внука до собственного Done. Когда счётчик достиг нуля, новых создателей уже нет.
func (m *childRunManager) wait() error {
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	return errors.Join(m.errors...)
}
