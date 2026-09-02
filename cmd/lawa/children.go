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
	"syscall"
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
    "cwd":{"type":"string","description":"Существующий разрешённый абсолютный рабочий каталог дочернего workflow."},
    "task":{"type":"string"},
    "taskFile":{"type":"string"},
    "parentRun":{"type":"string"}
  },
  "required":["workflow","cwd","parentRun"],
  "oneOf":[{"required":["task"]},{"required":["taskFile"]}]
}`)

var nativeChildTools = []codex.DynamicTool{
	{
		Name: "run_child", Description: "Надёжно зарегистрировать и запустить один дочерний workflow Lawa в указанном разрешённом абсолютном cwd; возвращает runId.",
		InputSchema: childInputSchema,
	},
	{
		Name: "run_children", Description: "Надёжно зарегистрировать и параллельно запустить дочерние workflow Lawa с независимыми разрешёнными абсолютными cwd; возвращает runIds.",
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
	input     runstore.Input
	directory *codex.Directory
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
	// registrationMu делает поиск и создание одним действием для разных callId
	// с одинаковой задачей. Внешний LockedRun уже запрещает два процесса для одного
	// parent; здесь закрывается гонка его параллельных кубиков внутри общего manager.
	// Блокировка не удерживается до завершения созданных run.
	registrationMu sync.Mutex
	mu             sync.Mutex
	calls          map[string]*childCall
	started        map[string]bool
	errors         []error
	wg             sync.WaitGroup
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
		child, resolveErr := m.resolve(ctx, parent, request)
		if resolveErr != nil {
			return "", errors.Join(fmt.Errorf("дочерний запуск %d: %w", index+1, resolveErr), closeResolvedChildren(resolved))
		}
		resolved = append(resolved, child)
	}
	key, err := prepareChildCall(parent.Meta.RunID, call, requests, resolved)
	if err != nil {
		return "", errors.Join(err, closeResolvedChildren(resolved))
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

// resolve читает workflow и taskFile из прежних доверенных корней, а cwd открывает
// независимо как файловую capability. Проверка подключения использует тот же
// дескриптор и выполняется до durable-регистрации: недоступный политике среды
// каталог не оставляет run, который заведомо нельзя запустить.
func (m *childRunManager) resolve(ctx context.Context, parent runstore.Snapshot, request childRequest) (_ resolvedChild, err error) {
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
	if !filepath.IsAbs(request.CWD) {
		return resolvedChild{}, errors.New("cwd дочернего workflow должен быть абсолютным путём")
	}
	directory, err := codex.OpenDirectory(request.CWD)
	if err != nil {
		return resolvedChild{}, fmt.Errorf("проверить cwd: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, directory.Close())
		}
	}()
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		return resolvedChild{}, fmt.Errorf("открыть workspace родителя: %w", err)
	}
	defer func() { err = errors.Join(err, workspaceRoot.Close()) }()
	runRoot, err := os.OpenRoot(runDir)
	if err != nil {
		return resolvedChild{}, fmt.Errorf("открыть папку родительского run: %w", err)
	}
	defer func() { err = errors.Join(err, runRoot.Close()) }()
	workflowJSON, err := readAllowedFile(workspaceRoot, runRoot, workspace, runDir, request.Workflow)
	if err != nil {
		return resolvedChild{}, fmt.Errorf("прочитать workflow: %w", err)
	}
	if _, err = workflow.Decode(bytes.NewReader(workflowJSON)); err != nil {
		return resolvedChild{}, fmt.Errorf("проверить workflow: %w", err)
	}
	task := request.Task
	if request.TaskFile != "" {
		data, readErr := readAllowedFile(workspaceRoot, runRoot, workspace, runDir, request.TaskFile)
		if readErr != nil {
			return resolvedChild{}, fmt.Errorf("прочитать taskFile: %w", readErr)
		}
		task = string(data)
	}
	if !utf8.ValidString(task) || strings.TrimSpace(task) == "" {
		return resolvedChild{}, errors.New("task должен быть непустым текстом UTF-8")
	}
	if m.deps.check == nil {
		return resolvedChild{}, errors.New("проверка подключения Codex не настроена")
	}
	connection := codex.Connection{
		Executable: m.executable, CWD: directory.Path(), Stderr: m.stderr, Directory: directory,
	}
	if err = m.deps.check(ctx, connection); err != nil {
		return resolvedChild{}, fmt.Errorf("cwd недоступен активной политике Codex: %w", err)
	}
	return resolvedChild{input: runstore.Input{
		WorkflowJSON: workflowJSON, Task: task, CWD: directory.Path(), ParentRunID: parent.Meta.RunID,
	}, directory: directory}, nil
}

// closeResolvedChildren освобождает только те capability, которые ещё не были
// переданы start. Метод Directory.Close допускает nil, поэтому частичный batch
// можно закрывать одним проходом без отдельной карты владения.
func closeResolvedChildren(children []resolvedChild) error {
	var err error
	for _, child := range children {
		err = errors.Join(err, child.directory.Close())
	}
	return err
}

// readAllowedFile выбирает один из двух доверенных корней по абсолютному имени,
// а само открытие выполняет через os.Root. В отличие от пары EvalSymlinks+ReadFile,
// Root удерживает файловую границу и не позволяет конкурентной подмене симлинка
// направить чтение наружу. Относительные пути, как и в CLI, относятся к workspace.
func readAllowedFile(workspaceRoot, runRoot *os.Root, workspace, runDir, path string) ([]byte, error) {
	root, relative := workspaceRoot, filepath.Clean(path)
	if filepath.IsAbs(path) {
		clean := filepath.Clean(path)
		var base string
		switch {
		case inside(workspace, clean):
			base = workspace
		case inside(runDir, clean):
			root, base = runRoot, runDir
		default:
			return nil, errors.New("файл должен находиться внутри workspace или родительского run")
		}
		var err error
		if relative, err = filepath.Rel(base, clean); err != nil {
			return nil, err
		}
	}
	return readRegularFile(root, relative)
}

// readRegularFile проверяет тип уже открытого объекта и читает тот же дескриптор.
// O_NONBLOCK не влияет на обычный файл, но не даёт созданному агентом FIFO зависнуть
// внутри Open до проверки типа. Поэтому между проверкой и чтением нельзя подставить
// другой объект по тому же пути.
func readRegularFile(root *os.Root, path string) (_ []byte, err error) {
	file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("путь не является обычным файлом")
	}
	return io.ReadAll(file)
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

// prepareChildCall разделяет две формы повтора. Стабильные адреса App Server
// образуют идентичность одного tool call, а смысловой вход входит в in-memory key,
// чтобы повреждённая повторная доставка с другими аргументами не получила старый
// ответ. Идентификатор ребёнка включает исходный структурированный запрос, а не
// прочитанное содержимое файлов: повторная доставка остаётся той же операцией,
// даже если workflow или taskFile успели измениться. Одинаковые элементы одного
// batch намеренно получают один durable request ID.
func prepareChildCall(parentRunID string, call codex.DynamicToolCall, requests []childRequest, children []resolvedChild) (string, error) {
	if strings.TrimSpace(call.CallID) == "" {
		return "", errors.New("item/tool/call не содержит callId")
	}
	if len(requests) != len(children) {
		return "", errors.New("число исходных и разрешённых дочерних запросов различается")
	}
	identityData, err := json.Marshal(struct {
		ParentRunID, ThreadID, TurnID, CallID, Tool string
	}{parentRunID, call.ThreadID, call.TurnID, call.CallID, call.Tool})
	if err != nil {
		return "", err
	}
	identitySum := sha256.Sum256(identityData)
	identity := hex.EncodeToString(identitySum[:])
	batchFingerprint, err := childFingerprint(children)
	if err != nil {
		return "", err
	}
	for index := range children {
		requestData, marshalErr := json.Marshal(struct {
			Call    string
			Request childRequest
		}{identity, requests[index]})
		if marshalErr != nil {
			return "", marshalErr
		}
		requestSum := sha256.Sum256(requestData)
		children[index].input.ChildRequestID = hex.EncodeToString(requestSum[:16])
	}
	return identity + ":" + batchFingerprint, nil
}

// launchOnce сериализует одинаковые запросы и повторно использует уже
// опубликованный child из обычного runstore. Create возвращается только после
// fsync, поэтому runId отдаётся агенту не раньше надёжной регистрации.
func (m *childRunManager) launchOnce(ctx context.Context, key string, children []resolvedChild) ([]string, error) {
	// Каждый Directory либо передаётся start, либо закрывается при любом раннем
	// выходе. Это особенно важно для повторной доставки уже известного callId:
	// свежий preflight открыл новые дескрипторы, но существующий запуск их не ждёт.
	defer func() { _ = closeResolvedChildren(children) }()
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
	m.registrationMu.Lock()
	defer m.registrationMu.Unlock()

	inputs := make([]runstore.Input, len(children))
	for index := range children {
		inputs[index] = children[index].input
	}
	snapshots, found, err := runstore.FindMatchingChildren(m.root, inputs)
	created := make(map[string]runstore.Snapshot)
	if err != nil {
		call.err = fmt.Errorf("найти дочерние run: %w", err)
	}
	for index, child := range children {
		if call.err != nil {
			break
		}
		snapshot, exists := snapshots[index], found[index]
		if !exists {
			if previous, ok := created[child.input.ChildRequestID]; ok {
				snapshot, exists = previous, true
			} else {
				snapshot, err = runstore.Create(m.root, child.input)
				if err == nil {
					created[child.input.ChildRequestID] = snapshot
				}
			}
		}
		if err != nil {
			call.err = fmt.Errorf("зарегистрировать дочерний run после %v: %w", call.runIDs, err)
			break
		}
		// Результат сохраняет позиционное соответствие batch: одинаковые входы
		// получают одинаковый ID дважды, но start ниже всё равно идемпотентен.
		call.runIDs = append(call.runIDs, snapshot.Meta.RunID)
		m.start(snapshot.Meta.RunID, exists, child.directory)
		children[index].directory = nil
	}
	m.mu.Lock()
	close(call.done)
	// Ошибка регистрации не является готовым ответом: повтор того же call должен
	// найти уже опубликованные элементы batch и попробовать закончить остальные.
	if call.err != nil {
		delete(m.calls, key)
	}
	m.mu.Unlock()
	return append([]string(nil), call.runIDs...), call.err
}

func (m *childRunManager) start(runID string, resume bool, directory *codex.Directory) {
	m.mu.Lock()
	if m.started[runID] {
		m.mu.Unlock()
		_ = directory.Close()
		return
	}
	m.started[runID] = true
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer directory.Close()
		_, err := coordinateRunWithOutcome(m.ctx, m.root, runID, m.executable, m.pool, io.Discard, m.stderr, m.deps, resume, true, m, directory)
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
