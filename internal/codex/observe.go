package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Connection задаёт исполняемый файл и рабочую папку для безопасных операций,
// которые не создают чат. Пустой Executable, как и в Command, означает codex
// из PATH. Stderr получает диагностику отдельного app-server.
type Connection struct {
	Executable, CWD string
	Stderr          io.Writer
	Permissions     *PermissionProfile
}

// WorkStatus — нормализованный результат наблюдения сохранённого чата. Он не
// зависит от внутренних состояний планировщика, чтобы пакет codex оставался
// самостоятельным клиентом внешнего протокола.
type WorkStatus string

const (
	WorkUnknown            WorkStatus = "unknown"
	WorkRunning            WorkStatus = "running"
	WorkWaitingForApproval WorkStatus = "waiting_for_approval"
	WorkFailed             WorkStatus = "failed"
	WorkInterrupted        WorkStatus = "interrupted"
	WorkCompleted          WorkStatus = "completed"
)

// Observation хранит минимальный снимок thread/read, необходимый координатору.
// LatestTurnID полезен для диагностики ручного продолжения, но Lawa намеренно
// не сохраняет полную историю или текст ответов агента.
type Observation struct {
	ThreadID, CWD, ThreadStatus, LatestTurnID, LatestTurnStatus string
	ActiveFlags                                                 []string
	LatestTurnError                                             *TurnError
}

// Check подтверждает, что app-server запускается и принимает обязательное
// initialize/initialized. Он не создаёт, не продолжает и не изменяет чаты.
func Check(ctx context.Context, connection Connection) (err error) {
	if err = validateConnection(ctx, connection); err != nil {
		return err
	}
	result := Result{}
	s, err := openSession(ctx, Command{
		Executable: connection.Executable,
		CWD:        connection.CWD,
		Stderr:     connection.Stderr,
	}, &result)
	if err != nil {
		return err
	}
	return errors.Join(s.Close(), ctx.Err())
}

// Inspect читает актуальное состояние сохранённого чата без запуска нового
// turn. includeTurns нужен, чтобы отличить успешный последний ручной повтор от
// прежней ошибки. Полный текст items игнорируется и нигде не сохраняется.
func Inspect(ctx context.Context, connection Connection, threadID string) (observation Observation, err error) {
	if err = validateConnection(ctx, connection); err != nil {
		return observation, err
	}
	if !validProtocolText(threadID) {
		return observation, errors.New("нужен непустой ID чата Codex в UTF-8")
	}
	result := Result{}
	s, err := openSession(ctx, Command{
		Executable: connection.Executable,
		CWD:        connection.CWD,
		Stderr:     connection.Stderr,
	}, &result)
	if err != nil {
		return observation, err
	}
	defer func() { err = errors.Join(err, s.Close(), ctx.Err()) }()
	var response struct {
		Thread struct {
			ID, CWD string
			Status  struct {
				Type        string
				ActiveFlags []string
			}
			Turns []struct {
				ID, Status string
				Error      *TurnError
			}
		}
	}
	if err = s.client.call("thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &response); err != nil {
		return observation, err
	}
	if response.Thread.ID != threadID {
		return observation, fmt.Errorf("Codex вернул чат %q вместо %q", response.Thread.ID, threadID)
	}
	if same, pathErr := sameDirectory(response.Thread.CWD, connection.CWD); pathErr != nil {
		return observation, fmt.Errorf("проверить cwd чата %q: %w", threadID, pathErr)
	} else if !same {
		return observation, fmt.Errorf("чат %q относится к cwd %q, ожидался %q", threadID, response.Thread.CWD, connection.CWD)
	}
	observation = Observation{
		ThreadID:     response.Thread.ID,
		CWD:          response.Thread.CWD,
		ThreadStatus: response.Thread.Status.Type,
		ActiveFlags:  append([]string(nil), response.Thread.Status.ActiveFlags...),
	}
	if turns := response.Thread.Turns; len(turns) != 0 {
		latest := turns[len(turns)-1]
		observation.LatestTurnID = latest.ID
		observation.LatestTurnStatus = latest.Status
		observation.LatestTurnError = latest.Error
	}
	if _, err = observation.Status(); err != nil {
		return Observation{}, fmt.Errorf("чат %q: %w", threadID, err)
	}
	return observation, nil
}

// Interrupt адресно отменяет только указанный выполняющийся turn. Отдельный
// app-server сначала присоединяется к сохранённому thread, поэтому вызов работает
// и для turn, запущенного другой stdio-сессией Lawa. Успешный RPC означает, что
// сервер принял отмену; терминальный status=interrupted получает исходный Run.
func Interrupt(ctx context.Context, connection Connection, threadID, turnID string) (err error) {
	if err = validateConnection(ctx, connection); err != nil {
		return err
	}
	if !validProtocolText(threadID) || !validProtocolText(turnID) {
		return errors.New("для отмены нужны непустые ID чата и turn Codex в UTF-8")
	}
	result := Result{ThreadID: threadID, TurnID: turnID}
	command := Command{
		Executable: connection.Executable, CWD: connection.CWD,
		Stderr: connection.Stderr, Permissions: connection.Permissions,
	}
	s, err := openSession(ctx, command, &result)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.Close(), ctx.Err()) }()
	if err = resumeThread(s.client, command, threadID); err != nil {
		return err
	}
	return s.client.call("turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, nil)
}

// Status сопоставляет расширяемый внешний протокол с консервативным состоянием
// Lawa. Активный чат имеет приоритет над старым терминальным turn: пользователь
// мог вручную начать продолжение сразу после прежней ошибки или успеха.
func (o Observation) Status() (WorkStatus, error) {
	switch o.ThreadStatus {
	case "active":
		waiting := false
		for _, flag := range o.ActiveFlags {
			switch flag {
			case "waitingOnApproval", "waitingOnUserInput":
				waiting = true
			default:
				return "", fmt.Errorf("неизвестный активный флаг Codex %q", flag)
			}
		}
		if waiting {
			return WorkWaitingForApproval, nil
		}
		return WorkRunning, nil
	case "systemError":
		return WorkFailed, nil
	case "idle", "notLoaded":
		// Терминальный turn из сохранённой истории достовернее того, загружен
		// ли чат именно в этом короткоживущем процессе app-server.
		switch o.LatestTurnStatus {
		case "completed":
			return WorkCompleted, nil
		case "failed":
			return WorkFailed, nil
		case "interrupted":
			return WorkInterrupted, nil
		case "inProgress":
			return WorkRunning, nil
		case "":
			return WorkUnknown, nil
		default:
			return "", fmt.Errorf("неизвестный статус turn Codex %q", o.LatestTurnStatus)
		}
	default:
		return "", fmt.Errorf("неизвестный статус чата Codex %q", o.ThreadStatus)
	}
}

// validateConnection выполняется до запуска дочернего процесса, чтобы ошибка
// пользователя не выглядела как сбой Codex и не имела сетевых побочных эффектов.
func validateConnection(ctx context.Context, connection Connection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(connection.CWD) || !utf8.ValidString(connection.CWD+connection.Executable) {
		return errors.New("подключение Codex требует абсолютный cwd и параметры в UTF-8")
	}
	info, err := os.Stat(connection.CWD)
	if err != nil {
		return fmt.Errorf("проверить cwd: %w", err)
	}
	if !info.IsDir() {
		return errors.New("cwd должен быть папкой")
	}
	return nil
}

// validProtocolText проверяет обязательный внешний ID без предположения о его
// формате: протокол сейчас использует UUIDv7, но строковый контракт расширяем.
func validProtocolText(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != ""
}

// sameDirectory учитывает системные симлинки вроде /var -> /private/var на
// macOS. Ошибка EvalSymlinks значима: resume не должен наблюдать похожий путь,
// если сохранённая рабочая папка исчезла или была подменена.
func sameDirectory(first, second string) (bool, error) {
	first, err := filepath.EvalSymlinks(first)
	if err != nil {
		return false, err
	}
	second, err = filepath.EvalSymlinks(second)
	if err != nil {
		return false, err
	}
	return filepath.Clean(first) == filepath.Clean(second), nil
}
