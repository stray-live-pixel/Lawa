package desktop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// hostsContent заменяет только упоминания точного local.lawa.app. Другие имена
// на общей строке и комментарии сохраняются. Одна каноническая IPv4-запись
// соответствует listener 127.0.0.1; старый ::1 не должен вести в пустой порт.
func hostsContent(data []byte) []byte {
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		body, comment, hasComment := strings.Cut(line, "#")
		fields := strings.Fields(body)
		kept := []string{}
		found := false
		for i, field := range fields {
			if i > 0 && strings.EqualFold(strings.TrimSuffix(field, "."), Host) {
				found = true
			} else {
				kept = append(kept, field)
			}
		}
		if !found {
			lines = append(lines, line)
			continue
		}
		if len(kept) > 1 {
			line = strings.Join(kept, "\t")
		} else {
			line = ""
		}
		if hasComment && strings.TrimSpace(comment) != "Lawa" {
			line += " #" + comment
		}
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n127.0.0.1\t" + Host + " # Lawa\n")
}

// RegisterHosts — узкий привилегированный entrypoint без пользовательских путей.
// Этот шаг и создание Applications выполняются с правами root; сервер остаётся
// пользовательским. Фиксированный /private/etc избегает symlink /etc на macOS.
func RegisterHosts() error {
	if runtime.GOOS != "darwin" || os.Geteuid() != 0 {
		return fmt.Errorf("desktop-hosts требует macOS и права администратора")
	}
	path := "/private/etc/hosts"
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated := hostsContent(data)
	if string(data) == string(updated) {
		return nil
	}
	return writeFile(path, updated, 0644)
}

// registerSystem запрашивает права штатным диалогом macOS. Доверие ограничено
// SSL и точным hostname; приватный ключ не передаётся Keychain. При повторной
// установке сертификат не добавляется повторно; права нужны для Applications.
func registerSystem(ctx context.Context, p Paths, executable string) error {
	data, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return err
	}
	cert := filepath.Join(p.Dir, "server.pem")
	trusted := command(ctx, "/usr/bin/security", "verify-cert", "-c", cert, "-p", "ssl", "-s", Host) == nil
	script := shellQuote(executable) + " desktop-app"
	if string(data) != string(hostsContent(data)) {
		script += " && " + shellQuote(executable) + " desktop-hosts"
	}
	if !trusted {
		script += " && /usr/bin/security add-trusted-cert -d -r trustRoot -p ssl -s " + shellQuote(Host) + " -k /Library/Keychains/System.keychain " + shellQuote(cert)
	}
	if err = command(ctx, "/usr/bin/osascript", "-e", "do shell script "+strconv.Quote(script)+" with administrator privileges"); err != nil {
		return err
	}
	if err = command(ctx, "/usr/bin/dscacheutil", "-flushcache"); err != nil {
		return err
	}
	return command(ctx, "/usr/bin/security", "verify-cert", "-c", cert, "-p", "ssl", "-s", Host)
}

// RegisterApp создаёт launcher по фиксированному системному пути. Его executable
// — тот же бинарник, из которого пользователь вызвал desktop-install.
func RegisterApp() error {
	if runtime.GOOS != "darwin" || os.Geteuid() != 0 {
		return fmt.Errorf("desktop-app требует macOS и права администратора")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return writeBundle("/Applications/Lawa.app", executable)
}
