// Package codex управляет официальными app-server-сессиями. Run и Continue
// выполняют один активный turn через отдельный процесс, а Observer переиспользует
// один read-only процесс для последовательных thread/read. Это единственный
// runtime Lawa. Интеграция с задачами Desktop намеренно отсутствует: публичного
// API управления ими нет, а агент-посредник добавил бы модельные turn, задержку,
// стоимость и узкое место массового параллелизма. Архитектурное решение описано в
// docs/codex-integration.md.
// Протокол: https://learn.chatgpt.com/docs/app-server.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Skill задаёт явный вызов доступного Codex скилла: имя без $, путь к его SKILL.md.
// Абсолютный путь сам по себе не устанавливает скилл и не включает отключённый.
// Run добавляет упоминание в текст и отдельный input типа skill по протоколу Codex.
type Skill struct{ Name, Path string }

// PermissionProfile описывает одноразовый именованный профиль для нового чата.
// :workspace сохраняет обычные права проекта, ReadPaths открывает служебный run
// только для чтения, а WritePaths даёт более узкие исключения, например ровно
// один файл памяти. Managed restrictions Codex продолжают ограничивать профиль.
type PermissionProfile struct {
	Name                  string
	ReadPaths, WritePaths []string
}

// DynamicTool описывает одну функцию, которую Lawa объявляет новому Codex
// thread через experimental dynamicTools. InputSchema обязан быть JSON Schema
// объекта. Само объявление не исполняет команду: каждый вызов отдельно приходит
// клиенту как item/tool/call и проходит CallDynamicTool.
type DynamicTool struct {
	Name, Description string
	InputSchema       json.RawMessage
}

// DynamicToolCall — проверенный адрес и аргументы одного item/tool/call.
// CallID стабилен для повторной доставки того же вызова и позволяет обработчику
// не создавать второй внешний ресурс, если ответ потерялся после первого раза.
type DynamicToolCall struct {
	ThreadID, TurnID, CallID, Tool string
	Arguments                      json.RawMessage
}

// Event сохраняет неизвестные поля Params: версии Codex могут добавлять события.
// ID непустой только у запроса сервера; его нельзя путать с ID нашего RPC.
type Event struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// ProcessEvent описывает только жизненный цикл собственного процесса App Server.
// Он не содержит командную строку или окружение: эти поля не нужны оператору и
// могли бы раскрыть локальные настройки. ExitCode отсутствует при завершении сигналом.
type ProcessEvent struct {
	Kind     string
	Time     time.Time
	PID      int
	ExitCode *int
	Signal   string
}

// Command описывает один новый запуск. Пустой Executable означает codex из PATH;
// shell не используется. Пустые Model, Effort и ServiceTier наследуются из Codex;
// непустые значения являются явными override одного кубика и повторяются при
// Continue, чтобы новый процесс app-server не изменил сохранённый контракт run.
// Непустой Sandbox явно передаётся серверу, который проверяет его против managed restrictions.
// Кубик автономен внутри sandbox. on-request + auto_review поручает оценку
// дополнительных прав самому Codex, не выдавая их автоматически из Lawa.
// Permissions создаёт одноразовый :workspace-профиль с точечными путями и
// взаимоисключён с Sandbox. Managed restrictions остаются обязательными; отказ
// возвращается без обхода.
// Stderr получает диагностику сервера; nil отбрасывает её, не накапливая в памяти.
// OnProcess вызывается после Start и после Wait собственного app-server. OnThread
// вызывается после получения ID, строго до отправки команды: координатор
// может сохранить связь и запретить turn своей ошибкой. OnTurn получает ID сразу
// после ответа turn/start вместе с одноразовой функцией interrupt. Эта функция
// отправляет turn/interrupt через ту же stdio-сессию, которая владеет активным
// чатом: второй app-server не нужен и не конкурирует за writer хранилища Codex.
// Notify и Respond синхронны, должны учитывать отмену и не вызывать Run или
// Continue рекурсивно для повтора той же задачи.
// DynamicTools и CallDynamicTool задают только явно разрешённые структурированные
// функции. Клиент проверяет thread, turn, имя функции и JSON-аргументы, а ошибка
// обработчика возвращается модели как неуспешный результат инструмента и не
// разрывает протокол App Server.
// Respond получает каждый прочий запрос сервера, кроме служебного currentTime/read.
// Обработчик обязан выбрать форму ответа по Event.Method и проверить Event.Params.
// Для неподдерживаемого метода он может вернуть InteractionRequired. Nil Respond
// делает это для любого такого запроса, не отправляя молчаливое согласие.
type Command struct {
	Executable, CWD, Text, Title, Sandbox string
	Model, Effort, ServiceTier            string
	Skill                                 *Skill
	Permissions                           *PermissionProfile
	Stderr                                io.Writer
	OnProcess                             func(ProcessEvent) error
	OnThread                              func(string) error
	OnTurn                                func(string, func(context.Context) error) error
	Notify                                func(Event) error
	Respond                               func(context.Context, Event) (any, error)
	DynamicTools                          []DynamicTool
	CallDynamicTool                       func(context.Context, DynamicToolCall) (string, error)
}

// Result сохраняет уже полученные ID даже при ошибке. Флаги попыток выставляются
// до записи RPC: при потере ответа нельзя считать, что действия не произошло.
// Пустые ID не разрешают повтор. Status=completed означает завершение turn без
// ошибки исполнения, но не проверяет смысл ответа. failed/interrupted — также
// нормальные результаты протокола, а не ошибка клиента. При failed поле
// TurnError сохраняет диагностический объект из терминального уведомления.
type Result struct {
	ThreadID, TurnID, Status         string
	CreationAttempted, TurnAttempted bool
	TurnError                        *TurnError
}

// TurnError описывает причину терминального статуса failed. CodexErrorInfo
// намеренно остаётся JSON: официальный протокол использует расширяемое объединение
// строковых кодов и объектов с параметрами, например HTTP-статусом. Сохранение
// исходного значения не заставляет клиента устаревать при добавлении новых видов
// ошибок. AdditionalDetails — указатель, чтобы отличать отсутствие поля от пустой
// строки; сам сервер может не прислать ни его, ни машинный код ошибки.
type TurnError struct {
	Message           string          `json:"message"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo,omitempty"`
	AdditionalDetails *string         `json:"additionalDetails,omitempty"`
}

// RPCError сохраняет весь отказ Codex без распознавания его текста и Data:
// JSON-RPC разрешает серверу расширять Data любым JSON. Вызывающий код может
// разобрать его после errors.As, но Error намеренно не пишет Data в логи,
// потому что там могут оказаться лишние для диагностики данные. Транспортные
// ошибки возвращаются отдельно. Ни один вид ошибки не запускает автоматический повтор.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error сохраняет код и текст отказа без интерпретации его как результата агента.
func (e *RPCError) Error() string { return fmt.Sprintf("Codex RPC %d: %s", e.Code, e.Message) }

// InteractionRequired означает, что сервер ждёт решения, а Respond не задан
// или явно отказался обслуживать этот Event.Method. Клиент останавливается
// с сохранёнными ID, не одобряет и не имитирует ответ UI.
type InteractionRequired struct{ Event Event }

// Error указывает, для какого запроса вызывающему коду нужен обработчик.
func (e *InteractionRequired) Error() string {
	return "Codex требует обработчика запроса " + e.Event.Method
}

// client читает поток только в горутине Run, но interrupt может записываться
// конкурентно из координатора. mu сериализует JSON-записи и выдачу RPC-ID, а
// pending принимает ответы на такие фоновые запросы обратно в единственном
// read-loop. encoding/json используется как потоковый codec, не net/rpc: здесь
// есть уведомления без ID и запросы сервера со строковыми или числовыми ID.
type client struct {
	ctx       context.Context
	command   Command
	input     *json.Encoder
	output    *json.Decoder
	mu        sync.Mutex
	nextID    int
	pending   map[int]chan envelope
	result    *Result
	completed map[string]turnCompletion // Завершение может прийти раньше ответа turn/start.
}

// turnCompletion хранит вместе статус и его диагностику: раздельные map могли бы
// дать вызывающему код статус failed без соответствующей ошибки при раннем событии.
type turnCompletion struct {
	Status string
	Error  *TurnError
}

// Run проверяет вход до запуска процесса, создаёт чат и сразу отправляет команду.
// До вызова координатор сохраняет собственное намерение создания; Run не пишет
// meta.json и не обещает идемпотентности между вызовами. Ошибка OnThread запрещает
// turn, но созданный чат не удаляется. Все остальные ошибки также не откатываются.
// Отмена ctx останавливает собственный процесс, а не Desktop; это может прервать
// агента. Nil error означает подтверждённый терминальный статус, не обязательно
// успех. Обработчики вызываются до возврата; фоновых горутин клиента не остаётся.
func Run(ctx context.Context, command Command) (result Result, err error) {
	if err = ctx.Err(); err != nil {
		return result, err
	}
	inputs, err := prepareTurn(command)
	if err != nil {
		return result, err
	}
	session, err := openSession(ctx, command, &result)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, session.Close(), ctx.Err()) }()
	c := session.client
	// Автономность относится только к этому чату; конфиг Desktop не изменяется.
	// Экспериментальный historyMode не передаём: серверный режим по умолчанию
	// поддерживает создание и resume через thread/read, тогда как paginated доступен не во
	// всех версиях app-server и пока не гарантирует полный жизненный цикл чата.
	params := map[string]any{"cwd": command.CWD, "approvalPolicy": "on-request", "approvalsReviewer": "auto_review"}
	if len(command.DynamicTools) != 0 {
		params["dynamicTools"] = dynamicToolSpecs(command.DynamicTools)
	}
	addRuntimeOverrides(params, command, false)
	if command.Permissions != nil {
		params["permissions"] = command.Permissions.Name
	} else if command.Sandbox != "" {
		params["sandbox"] = command.Sandbox
	}
	var created struct{ Thread struct{ ID string } }
	result.CreationAttempted = true
	if err = c.call("thread/start", params, &created); err != nil {
		return result, err
	}
	result.ThreadID = created.Thread.ID
	if result.ThreadID == "" {
		return result, errors.New("Codex не вернул ID созданного чата; повтор запрещён")
	}
	if command.OnThread != nil {
		if err = command.OnThread(result.ThreadID); err != nil {
			return result, fmt.Errorf("сохранить ID чата до запуска: %w", err)
		}
	}
	if command.Title != "" {
		if err = c.call("thread/name/set", map[string]any{"threadId": result.ThreadID, "name": command.Title}, nil); err != nil {
			return result, err
		}
	}
	err = startAndWait(c, command, inputs, &result)
	return result, err
}

// Continue открывает сохранённый чат и запускает в нём ровно один новый turn.
// В отличие от Run функция никогда не создаёт новый thread и не меняет его имя.
// Вызывающий код обязан предварительно проверить, что последний turn действительно
// interrupted: сама команда не принимает решение о допустимости автоматического
// продолжения и не повторяет неоднозначный turn/start.
func Continue(ctx context.Context, threadID string, command Command) (result Result, err error) {
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if !validProtocolText(threadID) {
		return result, errors.New("нужен непустой ID чата Codex в UTF-8")
	}
	// ID уже сохранён в meta.json и остаётся достоверным даже при ошибке
	// thread/resume. Координатор не должен принять такой сбой за создание нового
	// чата с неоднозначным результатом.
	result.ThreadID = threadID
	inputs, err := prepareTurn(command)
	if err != nil {
		return result, err
	}
	session, err := openSession(ctx, command, &result)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, session.Close(), ctx.Err()) }()
	if err = resumeThread(session.client, command, threadID); err != nil {
		return result, err
	}
	err = startAndWait(session.client, command, inputs, &result)
	return result, err
}

// prepareTurn проверяет общую часть Run и Continue до запуска app-server и
// формирует protocol input без shell-интерполяции пользовательского текста.
func prepareTurn(command Command) ([]map[string]any, error) {
	if !utf8.ValidString(command.Text) || strings.TrimSpace(command.Text) == "" {
		return nil, errors.New("нужна непустая команда в UTF-8")
	}
	if !filepath.IsAbs(command.CWD) {
		return nil, errors.New("нужен абсолютный cwd")
	}
	for _, value := range []string{command.CWD, command.Title, command.Sandbox} {
		if !utf8.ValidString(value) {
			return nil, errors.New("параметры Codex должны быть в UTF-8")
		}
	}
	for _, setting := range []struct{ name, value string }{
		{"model", command.Model}, {"effort", command.Effort}, {"service tier", command.ServiceTier},
	} {
		if setting.value != "" && (!utf8.ValidString(setting.value) || strings.IndexFunc(setting.value, unicode.IsSpace) >= 0) {
			return nil, fmt.Errorf("%s Codex должен быть непустым значением UTF-8 без пробельных символов", setting.name)
		}
	}
	if command.Permissions != nil {
		if command.Sandbox != "" {
			return nil, errors.New("именованный профиль permissions нельзя сочетать с sandbox")
		}
		if _, err := permissionOverride(command.Permissions); err != nil {
			return nil, err
		}
	}
	if err := validateDynamicTools(command); err != nil {
		return nil, err
	}
	info, err := os.Stat(command.CWD)
	if err != nil {
		return nil, fmt.Errorf("проверить cwd: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("cwd должен быть папкой")
	}
	text := command.Text
	inputs := []map[string]any{}
	if skill := command.Skill; skill != nil {
		if !utf8.ValidString(skill.Name+skill.Path) || strings.TrimSpace(skill.Name) == "" || strings.ContainsAny(skill.Name, "$ \t\r\n") || !filepath.IsAbs(skill.Path) {
			return nil, errors.New("скилл требует имени без $/пробелов и абсолютного пути")
		}
		text = "$" + skill.Name + " " + text
		inputs = append(inputs, map[string]any{"type": "skill", "name": skill.Name, "path": skill.Path})
	}
	return append([]map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}, inputs...), nil
}

// validateDynamicTools запрещает публиковать функцию без обработчика и
// неоднозначные повторные имена. Схема должна описывать объект: Lawa ожидает
// именованные поля и не принимает позиционные или скалярные аргументы.
func validateDynamicTools(command Command) error {
	if len(command.DynamicTools) == 0 {
		return nil
	}
	if command.CallDynamicTool == nil {
		return errors.New("dynamicTools требуют обработчик CallDynamicTool")
	}
	seen := make(map[string]bool, len(command.DynamicTools))
	for _, tool := range command.DynamicTools {
		if tool.Name == "" || strings.Trim(tool.Name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" || seen[tool.Name] {
			return fmt.Errorf("dynamicTools: неверное или повторное имя %q", tool.Name)
		}
		if !validProtocolText(tool.Description) {
			return fmt.Errorf("dynamicTools %q: нужно непустое описание UTF-8", tool.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema["type"] != "object" {
			return fmt.Errorf("dynamicTools %q: inputSchema должна быть JSON Schema объекта", tool.Name)
		}
		seen[tool.Name] = true
	}
	return nil
}

// dynamicToolSpecs переводит внутреннее описание в точную форму thread/start.
// RawMessage уже проверен выше и кодируется как JSON, а не строка со схемой.
func dynamicToolSpecs(tools []DynamicTool) []map[string]any {
	specs := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		specs = append(specs, map[string]any{
			"type": "function", "name": tool.Name,
			"description": tool.Description, "inputSchema": tool.InputSchema,
		})
	}
	return specs
}

// resumeThread загружает существующий thread в текущий app-server. Явные cwd,
// permissions и approval-настройки сохраняют тот же контур доступа, что был у
// исходного запуска; новый процесс не должен молча расширять или терять права.
func resumeThread(c *client, command Command, threadID string) error {
	params := map[string]any{
		"threadId": threadID, "cwd": command.CWD,
		"approvalPolicy": "on-request", "approvalsReviewer": "auto_review",
	}
	addRuntimeOverrides(params, command, false)
	if command.Permissions != nil {
		params["permissions"] = command.Permissions.Name
	} else if command.Sandbox != "" {
		params["sandbox"] = command.Sandbox
	}
	var resumed struct{ Thread struct{ ID, CWD string } }
	if err := c.call("thread/resume", params, &resumed); err != nil {
		return err
	}
	if resumed.Thread.ID != threadID {
		return fmt.Errorf("Codex возобновил чат %q вместо %q", resumed.Thread.ID, threadID)
	}
	if same, err := sameDirectory(resumed.Thread.CWD, command.CWD); err != nil {
		return fmt.Errorf("проверить cwd возобновлённого чата %q: %w", threadID, err)
	} else if !same {
		return fmt.Errorf("чат %q относится к cwd %q, ожидался %q", threadID, resumed.Thread.CWD, command.CWD)
	}
	return nil
}

// startAndWait отправляет один turn в уже загруженный thread и ждёт только его
// терминального события. OnTurn вызывается до ожидания и получает interrupt,
// связанный с этим client: координатор не угадывает ID и не открывает второй
// app-server, пока первый остаётся writer активного чата.
func startAndWait(c *client, command Command, inputs []map[string]any, result *Result) error {
	var started struct{ Turn struct{ ID string } }
	result.TurnAttempted = true
	// На 0.150.0-alpha.12.2 одного thread/start оказалось недостаточно: turn
	// сохранил прежнюю политику. Передаём явный override; sandbox не меняем.
	params := map[string]any{"threadId": result.ThreadID, "input": inputs, "approvalPolicy": "on-request", "approvalsReviewer": "auto_review"}
	addRuntimeOverrides(params, command, true)
	if err := c.call("turn/start", params, &started); err != nil {
		return err
	}
	result.TurnID = started.Turn.ID
	if result.TurnID == "" {
		return errors.New("Codex не вернул ID turn; повтор запрещён")
	}
	if command.OnTurn != nil {
		var once sync.Once
		var interruptErr error
		if err := command.OnTurn(result.TurnID, func(ctx context.Context) error {
			once.Do(func() {
				interruptErr = c.callAsync(ctx, "turn/interrupt", map[string]any{
					"threadId": result.ThreadID, "turnId": result.TurnID,
				})
			})
			return interruptErr
		}); err != nil {
			return fmt.Errorf("сохранить ID turn до ожидания: %w", err)
		}
	}
	for c.completed[result.TurnID].Status == "" {
		message, err := c.read()
		if err != nil {
			return fmt.Errorf("ожидание завершения Codex: %w", err)
		}
		if message.Method == "" {
			if c.deliverPending(message) {
				continue
			}
			return errors.New("ответ Codex без ожидающего RPC")
		}
	}
	completion := c.completed[result.TurnID]
	result.Status, result.TurnError = completion.Status, completion.Error
	return nil
}

// addRuntimeOverrides передаёт только явно заданные значения. Model и
// serviceTier поддерживаются на уровне thread/start и thread/resume; effort есть
// только у turn/start. Повтор всех трёх в turn/start делает первый и продолженный
// turn одинаковыми и оставляет проверку совместимости авторитетному app-server.
func addRuntimeOverrides(params map[string]any, command Command, includeEffort bool) {
	if command.Model != "" {
		params["model"] = command.Model
	}
	if includeEffort && command.Effort != "" {
		params["effort"] = command.Effort
	}
	if command.ServiceTier != "" {
		params["serviceTier"] = command.ServiceTier
	}
}

// send проверяет отмену перед каждой записью, включая ответы на встречные запросы.
// Все записи сериализованы: encoding/json.Encoder не обещает concurrent safety.
func (c *client) send(value any) error {
	return c.sendContext(c.ctx, value)
}

func (c *client) sendContext(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}
	return c.input.Encode(value)
}

// call не повторяет запрос при EOF, отмене, отказе сервера или неверном ответе.
func (c *client) call(method string, params, result any) error {
	requestID, _, err := c.startCall(c.ctx, method, params, false)
	if err != nil {
		return fmt.Errorf("Codex %s: %w", method, err)
	}
	for {
		message, err := c.read()
		if err != nil {
			return fmt.Errorf("Codex %s: %w", method, err)
		}
		if message.Method != "" {
			continue
		}
		if c.deliverPending(message) {
			continue
		}
		if string(message.ID) != strconv.Itoa(requestID) || len(message.Result) == 0 && message.Error == nil {
			return fmt.Errorf("Codex %s: неожиданный ответ RPC", method)
		}
		if message.Error != nil {
			return fmt.Errorf("Codex %s: %w", method, message.Error)
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(message.Result, result); err != nil {
			return fmt.Errorf("Codex %s: прочитать результат: %w", method, err)
		}
		return nil
	}
}

// callAsync записывает запрос в текущую сессию и ждёт его ответ, пока основной
// read-loop продолжает принимать события turn. Канал буферизован: если turn успел
// завершиться и внешний ctx отменился раньше ответа interrupt, доставка ответа не
// блокирует чтение и закрытие сессии.
func (c *client) callAsync(ctx context.Context, method string, params any) error {
	_, response, err := c.startCall(ctx, method, params, true)
	if err != nil {
		return fmt.Errorf("Codex %s: %w", method, err)
	}
	select {
	case message := <-response:
		if len(message.Result) == 0 && message.Error == nil {
			return fmt.Errorf("Codex %s: неожиданный ответ RPC", method)
		}
		if message.Error != nil {
			return fmt.Errorf("Codex %s: %w", method, message.Error)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

// startCall атомарно выдаёт ID и пишет запрос. Для фонового RPC ответ заранее
// регистрируется до Encode: быстрый app-server не сможет прислать ответ раньше,
// чем read-loop узнает, куда его доставить.
func (c *client) startCall(ctx context.Context, method string, params any, asynchronous bool) (int, <-chan envelope, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if err := c.ctx.Err(); err != nil {
		return 0, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if err := c.ctx.Err(); err != nil {
		return 0, nil, err
	}
	c.nextID++
	requestID := c.nextID
	var response chan envelope
	if asynchronous {
		response = make(chan envelope, 1)
		c.pending[requestID] = response
	}
	if err := c.input.Encode(map[string]any{"id": requestID, "method": method, "params": params}); err != nil {
		delete(c.pending, requestID)
		return 0, nil, err
	}
	return requestID, response, nil
}

// deliverPending отделяет ответ фонового interrupt от неожиданного ответа.
// Чтение остаётся в одной горутине, поэтому Decoder не нуждается в блокировке.
func (c *client) deliverPending(message envelope) bool {
	requestID, err := strconv.Atoi(string(message.ID))
	if err != nil {
		return false
	}
	c.mu.Lock()
	response := c.pending[requestID]
	delete(c.pending, requestID)
	c.mu.Unlock()
	if response == nil {
		return false
	}
	response <- message
	return true
}

// envelope отличает уведомление, встречный запрос и ответ на наш запрос.
type envelope struct {
	Event
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// read обслуживает один пакет. Неизвестные уведомления передаются Notify;
// неизвестные запросы без обработчика останавливают работу, не получают согласие.
func (c *client) read() (message envelope, err error) {
	if err = c.ctx.Err(); err != nil {
		return message, err
	}
	if err = c.output.Decode(&message); err != nil || message.Method == "" {
		return message, err
	}
	if len(message.ID) != 0 {
		var answer any
		if message.Method == "currentTime/read" {
			answer = map[string]int64{"currentTimeAt": time.Now().Unix()}
		} else if message.Method == "item/tool/call" && len(c.command.DynamicTools) != 0 {
			answer = c.respondDynamicTool(message.Params)
		} else if c.command.Respond == nil {
			return message, &InteractionRequired{message.Event}
		} else if answer, err = c.command.Respond(c.ctx, message.Event); err != nil {
			return message, err
		}
		return message, c.send(map[string]any{"id": message.ID, "result": answer})
	}
	if message.Method == "turn/completed" {
		var p struct {
			ThreadID string
			Turn     struct {
				ID, Status string
				Error      *TurnError
			}
		}
		if err = json.Unmarshal(message.Params, &p); err != nil {
			return message, err
		}
		if p.ThreadID == c.result.ThreadID && p.Turn.ID != "" && (c.result.TurnID == "" || p.Turn.ID == c.result.TurnID) {
			switch p.Turn.Status {
			case "completed", "failed", "interrupted":
				c.completed[p.Turn.ID] = turnCompletion{Status: p.Turn.Status, Error: p.Turn.Error}
			default:
				return message, fmt.Errorf("неизвестный терминальный статус Codex %q", p.Turn.Status)
			}
		}
	}
	if c.command.Notify != nil {
		err = c.command.Notify(message.Event)
	}
	return message, err
}

// respondDynamicTool проверяет, что вызов относится именно к текущему turn и к
// опубликованной функции. Любая ошибка становится success=false: модель видит
// понятный отказ и может исправить аргументы, а stdio-сессия остаётся валидной.
func (c *client) respondDynamicTool(raw json.RawMessage) any {
	var params struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		CallID    string          `json:"callId"`
		Namespace *string         `json:"namespace"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var result string
	var err error
	if decodeErr := json.Unmarshal(raw, &params); decodeErr != nil {
		err = fmt.Errorf("прочитать item/tool/call: %w", decodeErr)
	} else if params.ThreadID != c.result.ThreadID || params.TurnID != c.result.TurnID {
		err = errors.New("item/tool/call относится к другому thread или turn")
	} else if params.CallID == "" || params.Tool == "" || params.Namespace != nil {
		err = errors.New("item/tool/call содержит неверный callId, tool или namespace")
	} else if !c.hasDynamicTool(params.Tool) {
		err = fmt.Errorf("dynamic tool %q не объявлен", params.Tool)
	} else {
		var object map[string]any
		if decodeErr := json.Unmarshal(params.Arguments, &object); decodeErr != nil || object == nil {
			err = errors.New("arguments dynamic tool должны быть JSON-объектом")
		} else {
			result, err = c.command.CallDynamicTool(c.ctx, DynamicToolCall{
				ThreadID: params.ThreadID, TurnID: params.TurnID, CallID: params.CallID,
				Tool: params.Tool, Arguments: params.Arguments,
			})
			if err == nil && !validProtocolText(result) {
				err = errors.New("dynamic tool вернул пустой или неверный UTF-8 результат")
			}
		}
	}
	success := err == nil
	if err != nil {
		result = err.Error()
	}
	return map[string]any{
		"contentItems": []map[string]string{{"type": "inputText", "text": result}},
		"success":      success,
	}
}

func (c *client) hasDynamicTool(name string) bool {
	for _, tool := range c.command.DynamicTools {
		if tool.Name == name {
			return true
		}
	}
	return false
}
