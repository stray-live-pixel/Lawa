// Package codex выполняет одну команду через отдельный официальный app-server.
// Это клиент протокола, не координатор workflow: Run создаёт новый чат и ждёт
// терминальный статус одного turn. Закрытие клиента завершает его сервер; передача
// живой работы в Desktop и долговечный фоновый сервис в этот контракт не входят.
// Протокол: https://learn.chatgpt.com/docs/app-server.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Skill задаёт явный вызов доступного Codex скилла: имя без $, путь к его SKILL.md.
// Абсолютный путь сам по себе не устанавливает скилл и не включает отключённый.
// Run добавляет упоминание в текст и отдельный input типа skill по протоколу Codex.
type Skill struct{ Name, Path string }

// Event сохраняет неизвестные поля Params: версии Codex могут добавлять события.
// ID непустой только у запроса сервера; его нельзя путать с ID нашего RPC.
type Event struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Command описывает один новый запуск. Пустой Executable означает codex из PATH;
// shell не используется. Модель и права наследуются из Codex; непустой Sandbox
// явно передаётся серверу, который проверяет его против managed restrictions.
// Кубик автономен внутри sandbox. on-request + auto_review поручает оценку
// дополнительных прав самому Codex, не выдавая их автоматически из Lawa.
// Managed restrictions остаются обязательными; отказ возвращается без обхода.
// Stderr получает диагностику сервера; nil отбрасывает её, не накапливая в памяти.
// OnThread вызывается после получения ID, строго до отправки команды: координатор
// может сохранить связь и запретить turn своей ошибкой. Notify и Respond синхронны,
// должны учитывать отмену и не вызывать Run рекурсивно для повтора той же задачи.
// Respond обрабатывает запросы разрешений/ввода; nil запрещает молчаливое согласие.
type Command struct {
	Executable, CWD, Text, Title, Sandbox string
	Skill                                 *Skill
	Stderr                                io.Writer
	OnThread                              func(string) error
	Notify                                func(Event) error
	Respond                               func(context.Context, Event) (any, error)
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

// RPCError содержит отказ Codex без распознавания текста; транспортные ошибки
// возвращаются отдельно. Ни один вид ошибки не запускает автоматический повтор.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error сохраняет код и текст отказа без интерпретации его как результата агента.
func (e *RPCError) Error() string { return fmt.Sprintf("Codex RPC %d: %s", e.Code, e.Message) }

// InteractionRequired означает, что сервер ждёт решения, а Respond не задан.
// Клиент останавливается с сохранёнными ID, не одобряет и не имитирует ответ UI.
type InteractionRequired struct{ Event Event }

// Error указывает, для какого запроса вызывающему коду нужен обработчик.
func (e *InteractionRequired) Error() string {
	return "Codex требует обработчика запроса " + e.Event.Method
}

// client обслуживается только горутиной Run. Запросы идут последовательно, но
// между ответами читаются уведомления и встречные запросы, иначе Codex зависнет
// на согласовании. encoding/json используется как потоковый codec, не net/rpc:
// здесь есть уведомления без ID и запросы сервера со строковыми или числовыми ID.
type client struct {
	ctx       context.Context
	command   Command
	input     *json.Encoder
	output    *json.Decoder
	nextID    int
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
	if !utf8.ValidString(command.Text) || strings.TrimSpace(command.Text) == "" {
		return result, errors.New("нужна непустая команда в UTF-8")
	}
	if !filepath.IsAbs(command.CWD) {
		return result, errors.New("нужен абсолютный cwd")
	}
	for _, value := range []string{command.CWD, command.Title, command.Sandbox} {
		if !utf8.ValidString(value) {
			return result, errors.New("параметры Codex должны быть в UTF-8")
		}
	}
	info, err := os.Stat(command.CWD)
	if err != nil {
		return result, fmt.Errorf("проверить cwd: %w", err)
	}
	if !info.IsDir() {
		return result, errors.New("cwd должен быть папкой")
	}
	text := command.Text
	inputs := []map[string]any{}
	if skill := command.Skill; skill != nil {
		if !utf8.ValidString(skill.Name+skill.Path) || strings.TrimSpace(skill.Name) == "" || strings.ContainsAny(skill.Name, "$ \t\r\n") || !filepath.IsAbs(skill.Path) {
			return result, errors.New("скилл требует имени без $/пробелов и абсолютного пути")
		}
		text = "$" + skill.Name + " " + text
		inputs = append(inputs, map[string]any{"type": "skill", "name": skill.Name, "path": skill.Path})
	}
	inputs = append([]map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}, inputs...)
	if command.Executable == "" {
		command.Executable = "codex"
	}
	process := exec.CommandContext(ctx, command.Executable, "app-server", "--stdio")
	process.Dir, process.Stderr = command.CWD, command.Stderr
	process.WaitDelay = time.Second // Только очистка pipe после выхода, не лимит работы агента.
	stdin, err := process.StdinPipe()
	if err != nil {
		return result, err
	}
	defer stdin.Close()
	stdout, err := process.StdoutPipe()
	if err != nil {
		return result, err
	}
	defer stdout.Close()
	if err = process.Start(); err != nil {
		return result, fmt.Errorf("запустить Codex: %w", err)
	}
	defer func() { err = errors.Join(err, stop(process, stdin), ctx.Err()) }()
	c := client{ctx: ctx, command: command, input: json.NewEncoder(stdin), output: json.NewDecoder(stdout), result: &result, completed: map[string]turnCompletion{}}
	if err = c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "lawa", "version": "0.1.0"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
		return result, err
	}
	if err = c.send(map[string]any{"method": "initialized"}); err != nil {
		return result, err
	}
	// Автономность относится только к этому чату; конфиг Desktop не изменяется.
	// Экспериментальный historyMode не передаём: серверный legacy по умолчанию
	// поддерживает создание и будущий resume, тогда как paginated доступен не во
	// всех версиях app-server и пока не гарантирует полный жизненный цикл чата.
	params := map[string]any{"cwd": command.CWD, "approvalPolicy": "on-request", "approvalsReviewer": "auto_review"}
	if command.Sandbox != "" {
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
	var started struct{ Turn struct{ ID string } }
	result.TurnAttempted = true
	// На 0.150.0-alpha.12.2 одного thread/start оказалось недостаточно: turn
	// сохранил прежнюю политику. Передаём явный override; sandbox не меняем.
	if err = c.call("turn/start", map[string]any{"threadId": result.ThreadID, "input": inputs, "approvalPolicy": "on-request", "approvalsReviewer": "auto_review"}, &started); err != nil {
		return result, err
	}
	result.TurnID = started.Turn.ID
	if result.TurnID == "" {
		return result, errors.New("Codex не вернул ID turn; повтор запрещён")
	}
	for c.completed[result.TurnID].Status == "" {
		var message envelope
		if message, err = c.read(); err != nil {
			return result, fmt.Errorf("ожидание завершения Codex: %w", err)
		}
		if message.Method == "" {
			return result, errors.New("ответ Codex без ожидающего RPC")
		}
	}
	completion := c.completed[result.TurnID]
	result.Status, result.TurnError = completion.Status, completion.Error
	return result, nil
}

// send проверяет отмену перед каждой записью, включая запись после OnThread.
func (c *client) send(value any) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	return c.input.Encode(value)
}

// call не повторяет запрос при EOF, отмене, отказе сервера или неверном ответе.
func (c *client) call(method string, params, result any) error {
	c.nextID++
	if err := c.send(map[string]any{"id": c.nextID, "method": method, "params": params}); err != nil {
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
		if string(message.ID) != strconv.Itoa(c.nextID) || len(message.Result) == 0 && message.Error == nil {
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

// stop закрывает stdin и дожидается собственного процесса. Через секунду
// принудительно завершает только его; Desktop, чаты и настройки не удаляются.
// Wait вызывается после прекращения чтения stdout; его горутина всегда дождалась
// завершения к возврату. Ошибка штатного выхода сохраняется, наше Kill ожидаемо.
func stop(process *exec.Cmd, stdin io.Closer) error {
	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		_ = process.Process.Kill()
		<-done
		return nil
	}
}
