package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// session владеет одним процессом app-server и его stdio. Все публичные
// операции пакета создают отдельную сессию: так они не делят RPC-ID и события
// между параллельными кубиками, а сбой одного процесса не ломает остальные.
type session struct {
	client *client
	stdin  io.WriteCloser
	stdout io.ReadCloser
	proc   *exec.Cmd
}

// openSession запускает официальный app-server и завершает обязательное
// рукопожатие протокола. Вызов не создаёт чат и поэтому подходит как основа
// как для Run, так и для безопасной проверки подключения и thread/read.
func openSession(ctx context.Context, command Command, result *Result) (*session, error) {
	if command.Executable == "" {
		command.Executable = "codex"
	}
	arguments := []string{"app-server"}
	if command.Permissions != nil {
		override, err := permissionOverride(command.Permissions)
		if err != nil {
			return nil, err
		}
		arguments = append(arguments, "-c", override)
	}
	arguments = append(arguments, "--stdio")
	process := exec.CommandContext(ctx, command.Executable, arguments...)
	process.Dir, process.Stderr = command.CWD, command.Stderr
	// Терминальный SIGINT/SIGTERM предназначен координатору. Отдельная группа
	// не даёт ядру одновременно послать тот же сигнал дочернему app-server;
	// координатор сам адресует turn/interrupt и получает точный терминальный статус.
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// WaitDelay ограничивает только очистку pipe после выхода app-server. Это не
	// таймаут turn и не разрешение прерывать работу агента по времени.
	process.WaitDelay = time.Second
	stdin, err := process.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := process.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err = process.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("запустить Codex: %w", err)
	}
	s := &session{stdin: stdin, stdout: stdout, proc: process}
	s.client = &client{
		ctx:       ctx,
		command:   command,
		input:     json.NewEncoder(stdin),
		output:    json.NewDecoder(stdout),
		result:    result,
		completed: map[string]turnCompletion{},
	}
	if err = s.client.call("initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "lawa", "version": "0.1.0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, nil); err == nil {
		err = s.client.send(map[string]any{"method": "initialized"})
	}
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

// permissionOverride собирает один аргумент TOML без shell. Имена ограничены
// bare-key синтаксисом TOML, пути кодируются basic string. Более узкий write
// внутри read-каталога поддержан проверенным разрешителем профилей Codex.
func permissionOverride(profile *PermissionProfile) (string, error) {
	if profile == nil || profile.Name == "" || strings.Trim(profile.Name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" {
		return "", errors.New("профиль permissions требует имени из букв, цифр, _ и -")
	}
	if len(profile.ReadPaths) == 0 || len(profile.WritePaths) == 0 {
		return "", errors.New("профиль permissions требует пути чтения и записи")
	}
	seen := map[string]string{}
	entries := make([]string, 0, len(profile.ReadPaths)+len(profile.WritePaths))
	for _, group := range []struct {
		mode  string
		paths []string
	}{{"read", profile.ReadPaths}, {"write", profile.WritePaths}} {
		for _, path := range group.paths {
			if !filepath.IsAbs(path) || !validProtocolText(path) {
				return "", fmt.Errorf("профиль permissions: нужен абсолютный путь %q", path)
			}
			if previous := seen[path]; previous != "" {
				return "", fmt.Errorf("профиль permissions: путь %q повторён как %s и %s", path, previous, group.mode)
			}
			seen[path] = group.mode
			entries = append(entries, tomlString(path)+"="+tomlString(group.mode))
		}
	}
	return "permissions." + profile.Name + "={extends=\":workspace\",filesystem={" + strings.Join(entries, ",") + "}}", nil
}

// tomlString кодирует UTF-8 путь как TOML basic string. Управляющие символы
// экранируются явно; значение передаётся exec напрямую и не исполняется shell.
func tomlString(value string) string {
	var encoded strings.Builder
	encoded.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\':
			encoded.WriteString("\\\\")
		case '"':
			encoded.WriteString("\\\"")
		case '\b':
			encoded.WriteString("\\b")
		case '\t':
			encoded.WriteString("\\t")
		case '\n':
			encoded.WriteString("\\n")
		case '\f':
			encoded.WriteString("\\f")
		case '\r':
			encoded.WriteString("\\r")
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&encoded, "\\u%04X", r)
			} else {
				encoded.WriteRune(r)
			}
		}
	}
	encoded.WriteByte('"')
	return encoded.String()
}

// Close закрывает транспорт и дожидается только собственного app-server.
// Desktop, сохранённые чаты и настройки не удаляются. Через секунду зависший
// служебный процесс завершается принудительно; активный turn к этому моменту
// публичный Run уже обязан был получить терминальное событие либо ошибку.
func (s *session) Close() error {
	if s == nil || s.proc == nil {
		return nil
	}
	_ = s.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- s.proc.Wait() }()
	select {
	case err := <-done:
		_ = s.stdout.Close()
		s.proc = nil
		return err
	case <-time.After(time.Second):
		_ = s.proc.Process.Kill()
		<-done
		_ = s.stdout.Close()
		s.proc = nil
		return nil
	}
}
