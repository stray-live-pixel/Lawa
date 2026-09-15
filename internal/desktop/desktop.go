// Package desktop создаёт интеграцию macOS из единственного бинарника Lawa.
// launchd владеет сервером, а Applications содержит только созданный launcher.
// Системные команды вызываются лишь из Install/Open; тесты используют временные
// каталоги и локальные TLS-серверы без изменения настроек компьютера.
package desktop

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/dashboard"
)

// Host — локальное имя, для которого устанавливается ограниченное SSL-доверие.
const Host = "local.lawa.app"

// URL использует согласованный порт; loopback listener не доступен из сети.
const URL = "https://" + Host + ":60800"
const label = "app.lawa.local"
const healthPath = "/.lawa/ready"

// Paths привязывает все пользовательские артефакты к одному home. Корень run
// остаётся прежним; Finder и launchd не зависят от текущей папки и shell PATH.
type Paths struct{ Dir, App, Agent string }

// UserPaths возвращает системный launcher и пользовательские настройки.
func UserPaths(home string) Paths {
	return Paths{filepath.Join(home, "Library", "Application Support", "Lawa", "desktop"), "/Applications/Lawa.app", filepath.Join(home, "Library", "LaunchAgents", label+".plist")}
}

// Configured отличает отсутствие установки от повреждённого сертификата:
// существующий каталог никогда не означает разрешения откатиться на HTTP.
func Configured(home string) bool {
	_, err := os.Stat(UserPaths(home).Dir)
	return runtime.GOOS == "darwin" && !os.IsNotExist(err)
}

// Serve оставляет управление временем жизни существующему dashboard, добавляя
// TLS и проверку готовности. Этот же путь используется обычным lawa serve после
// установки: launcher может открыть и экземпляр, запущенный вручную.
func Serve(ctx context.Context, home, root string) error {
	p := UserPaths(home)
	pair, err := tls.LoadX509KeyPair(filepath.Join(p.Dir, "server.pem"), filepath.Join(p.Dir, "server.key"))
	if err != nil {
		return fmt.Errorf("HTTPS Lawa повреждён; повторите lawa desktop-install: %w", err)
	}
	listener, err := net.Listen("tcp", dashboard.DefaultAddress)
	if err != nil {
		return fmt.Errorf("порт 60800 занят; остановите прежний HTTP-сервер Lawa или освободите порт: %w", err)
	}
	return dashboard.ServeListener(ctx, tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}), desktopHandler(dashboard.Handler(root)))
}

// desktopHandler не читает run при проверке готовности: пустое хранилище также
// является рабочим UI. Маркер вместе с проверкой сертификата отличает Lawa от
// чужого HTTPS-сервера на том же порту.
func desktopHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == healthPath {
			w.Header().Set("X-Lawa-Ready", "1")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readyClient доверяет только локальному сертификату этой установки и всегда
// соединяется с loopback: proxy из окружения и DNS не участвуют в проверке.
func readyClient(p Paths) (*http.Client, error) {
	cert, err := os.ReadFile(filepath.Join(p.Dir, "server.pem"))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(cert)
	if block == nil {
		return nil, errors.New("неверный сертификат Lawa")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	if time.Now().After(parsed.NotAfter) {
		return nil, errors.New("срок сертификата Lawa истёк; выполните lawa desktop-install")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		return nil, errors.New("неверный сертификат Lawa")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: Host, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", dashboard.DefaultAddress)
	}}
	return &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("неожиданное перенаправление Lawa")
	}}, nil
}

// ready требует успешный TLS-запрос и собственный маркер готовности Lawa.
func ready(ctx context.Context, client *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, URL+healthPath, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.Header.Get("X-Lawa-Ready") != "1" {
		return errors.New("порт 60800 занят другим сервером")
	}
	return nil
}

// waitReady ограничивает ожидание запуска и сохраняет последнюю причину сбоя.
func waitReady(ctx context.Context, client *http.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		err := ready(ctx, client)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("UI не готов: %w; проверьте порт 60800 и desktop/server.log", err)
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// Open сначала ищет уже работающий экземпляр. kickstart без -k не перезапускает
// активный job: launchd сериализует даже одновременные нажатия нескольких иконок.
func Open(ctx context.Context, home string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("lawa ui поддерживается только на macOS")
	}
	p := UserPaths(home)
	client, err := readyClient(p)
	if err != nil {
		return fmt.Errorf("выполните lawa desktop-install: %w", err)
	}
	defer client.CloseIdleConnections()
	return openReady(ctx, client, func() error {
		return command(ctx, "/bin/launchctl", "kickstart", service())
	}, func() error { return command(ctx, "/usr/bin/open", URL) })
}

// openReady разделяет проверку сервера, запрос запуска и открытие браузера.
// Успешная проверка существующего сервера вообще не вызывает launchctl.
func openReady(ctx context.Context, client *http.Client, start, open func() error) error {
	if ready(ctx, client) != nil {
		if err := start(); err != nil {
			return fmt.Errorf("запустить Lawa; повторите lawa desktop-install: %w", err)
		}
		if err := waitReady(ctx, client); err != nil {
			return err
		}
	}
	return open()
}

// service адресует job текущей GUI-сессии, не системный root-daemon.
func service() string { return "gui/" + strconv.Itoa(os.Getuid()) + "/" + label }

// command передаёт аргументы напрямую, без shell; диагностический вывод нужен
// пользователю при отказе launchd, Keychain или регистрации Applications.
func command(ctx context.Context, name string, args ...string) error {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Alert показывает ошибку Finder-запуска, у которого нет видимого stderr.
func Alert(message string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = command(ctx, "/usr/bin/osascript", "-e", "display alert \"Lawa\" message "+strconv.Quote(message)+" as critical")
}

// Install регистрирует один job и один launcher. При обновлении launchd получает
// новый executable по стабильному пути, а сертификат и ключ сохраняются. Ошибка
// не маскируется: повторный вызов завершает частично выполненную установку.
func Install(ctx context.Context, home, executable string, out io.Writer) error {
	if runtime.GOOS != "darwin" {
		return errors.New("desktop-install поддерживается только на macOS")
	}
	if os.Getuid() == 0 {
		return errors.New("запускайте desktop-install обычным пользователем; права администратора будут запрошены отдельно")
	}
	p := UserPaths(home)
	if err := os.MkdirAll(p.Dir, 0700); err != nil {
		return err
	}
	if err := ensureCertificate(p, time.Now()); err != nil {
		return err
	}
	if err := writeAgent(p, executable); err != nil {
		return err
	}
	if err := registerSystem(ctx, p, executable); err != nil {
		return err
	}
	// bootout нужен при обновлении plist и бинарника. Отсутствующий job допустим;
	// остальные ошибки bootout не скрываются, если job ещё зарегистрирован.
	if command(ctx, "/bin/launchctl", "print", service()) == nil {
		if err := command(ctx, "/bin/launchctl", "bootout", service()); err != nil {
			return err
		}
	}
	if err := command(ctx, "/bin/launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), p.Agent); err != nil {
		return err
	}
	if err := command(ctx, "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", "-f", p.App); err != nil {
		return err
	}
	client, err := readyClient(p)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if err = waitReady(ctx, client); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Lawa: %s\nUI: %s\n", p.App, URL)
	return err
}
