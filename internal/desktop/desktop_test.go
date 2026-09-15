package desktop

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHostsPreservesAliasesAndIsIdempotent защищает чужие aliases/comments и
// повторную установку поверх прежних IPv4/IPv6-регистраций.
func TestHostsPreservesAliasesAndIsIdempotent(t *testing.T) {
	before := []byte("127.0.0.1 localhost\n::1 local.lawa.app other # keep\n10.0.0.1 LOCAL.LAWA.APP. # old\n# local.lawa.app comment\n")
	after := hostsContent(before)
	want := "127.0.0.1 localhost\n::1\tother # keep\n # old\n# local.lawa.app comment\n127.0.0.1\tlocal.lawa.app # Lawa\n"
	if string(after) != want {
		t.Fatalf("hosts: %q", after)
	}
	if !bytes.Equal(after, hostsContent(after)) {
		t.Fatal("повторная установка меняет hosts")
	}
}

// TestCertificateSurvivesUpdate проверяет настоящее TLS-доверие, имя host,
// отсутствие полномочий CA и сохранение байтов при обычном обновлении.
func TestCertificateSurvivesUpdate(t *testing.T) {
	p := UserPaths(t.TempDir())
	p.App = filepath.Join(p.Dir, "Lawa.app")
	if err := os.MkdirAll(p.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := ensureCertificate(p, now); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(filepath.Join(p.Dir, "server.pem"))
	if err := ensureCertificate(p, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(p.Dir, "server.pem"))
	if !bytes.Equal(original, again) {
		t.Fatal("сертификат изменён")
	}
	block, _ := pem.Decode(original)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: Host}); err != nil {
		t.Fatal(err)
	}
	if cert.IsCA || cert.VerifyHostname("example.com") == nil {
		t.Fatal("сертификат должен относиться только к Lawa")
	}
	info, _ := os.Stat(filepath.Join(p.Dir, "server.key"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("ключ доступен другим пользователям")
	}
	// Обновление в последние 30 дней перевыпускает сертификат до его истечения.
	if err = ensureCertificate(p, now.Add(340*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	renewed, _ := os.ReadFile(filepath.Join(p.Dir, "server.pem"))
	if bytes.Equal(original, renewed) {
		t.Fatal("сертификат не обновлён перед истечением")
	}
	// Неполная пара после сбоя не должна автоматически терять прежнее доверие.
	if err = os.Remove(filepath.Join(p.Dir, "server.key")); err != nil {
		t.Fatal(err)
	}
	if ensureCertificate(p, now) == nil {
		t.Fatal("потерянный ключ скрыт")
	}
}

// TestReadyAuthenticatesServer использует реальный TLS handshake на случайном
// порту. Проверяем валидный сервер и чужой ответ, не ослабляя TLS-проверку.
func TestReadyAuthenticatesServer(t *testing.T) {
	p := UserPaths(t.TempDir())
	p.App = filepath.Join(p.Dir, "Lawa.app")
	_ = os.MkdirAll(p.Dir, 0700)
	if err := ensureCertificate(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(p.Dir, "server.pem"), filepath.Join(p.Dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(desktopHandler(http.NotFoundHandler()))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	server.StartTLS()
	defer server.Close()
	client, err := readyClient(p)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	if err = ready(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	// Сертификат другой установки не проходит аутентификацию, даже если handler
	// возвращает верный маркер. Это защищает от открытия постороннего сервиса.
	other := httptest.NewTLSServer(desktopHandler(http.NotFoundHandler()))
	defer other.Close()
	client.CloseIdleConnections()
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", other.Listener.Addr().String())
	}
	if ready(context.Background(), client) == nil {
		t.Fatal("чужой сертификат принят")
	}
}

// TestLauncherExecutesExactBinary доказывает, что пробелы, апострофы и shell
// метасимволы в пути не меняют команду; обновление не создаёт дубликат launcher.
func TestLauncherExecutesExactBinary(t *testing.T) {
	p := UserPaths(t.TempDir())
	p.App = filepath.Join(p.Dir, "Lawa.app")
	_ = os.MkdirAll(p.Dir, 0700)
	executable := filepath.Join(p.Dir, "a ' $d `x` & lawa")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := testArtifacts(p, executable); err != nil {
			t.Fatal(err)
		}
	}
	output, err := exec.Command(filepath.Join(p.App, "Contents", "MacOS", "Lawa")).CombinedOutput()
	if err != nil || string(output) != "ui\n--finder\n" {
		t.Fatalf("launcher: %v %s", err, output)
	}
	icon, err := os.ReadFile(filepath.Join(p.App, "Contents", "Resources", "Lawa.icns"))
	if err != nil {
		t.Fatal(err)
	}
	if string(icon[:4]) != "icns" || int(binary.BigEndian.Uint32(icon[4:8])) != len(icon) {
		t.Fatal("неверный ICNS")
	}
	plist, _ := os.ReadFile(p.Agent)
	if !strings.Contains(string(plist), xmlText(executable)) {
		t.Fatal("путь в plist не экранирован")
	}
}

// TestReadyRejectsUnrelatedHTTPResponse проверяет, что успешного статуса HTTP
// недостаточно; отменённое ожидание заканчивается без открытия браузера.
func TestReadyRejectsUnrelatedHTTPResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "other app") }))
	defer server.Close()
	client := &http.Client{Transport: rewriteTransport{server.URL}}
	if ready(context.Background(), client) == nil {
		t.Fatal("чужой handler принят")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitReady(ctx, client) == nil {
		t.Fatal("отмена потеряна")
	}
}

// rewriteTransport перенаправляет тестовый запрос на случайный порт, сохраняя
// контекст. Production transport всегда использует loopback:60800 и TLS.
type rewriteTransport struct{ url string }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rewritten, err := http.NewRequestWithContext(req.Context(), req.Method, r.url+healthPath, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(rewritten)
}

// testArtifacts собирает обе части интеграции в изолированной папке.
func testArtifacts(p Paths, executable string) error {
	if err := writeBundle(p.App, executable); err != nil {
		return err
	}
	return writeAgent(p, executable)
}

// TestOpenReusesServerAndReportsFailure защищает сценарий повторного нажатия:
// браузер открывается каждый раз, start вызывается только для неготового UI.
func TestOpenReusesServerAndReportsFailure(t *testing.T) {
	var running atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if running.Load() {
			w.Header().Set("X-Lawa-Ready", "1")
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	client := &http.Client{Transport: rewriteTransport{server.URL}}
	starts, opens := 0, 0
	start := func() error { starts++; running.Store(true); return nil }
	open := func() error {
		if !running.Load() {
			t.Fatal("браузер открыт до готовности")
		}
		opens++
		return nil
	}
	for i := 0; i < 2; i++ {
		if err := openReady(context.Background(), client, start, open); err != nil {
			t.Fatal(err)
		}
	}
	if starts != 1 || opens != 2 {
		t.Fatalf("запуски %d, открытия %d", starts, opens)
	}
	running.Store(false)
	if openReady(context.Background(), client, func() error { return errors.New("launchd отказал") }, open) == nil {
		t.Fatal("ошибка запуска скрыта")
	}
	if opens != 2 {
		t.Fatal("браузер открыт после отказа запуска")
	}
}

// TestBusyPortRemainsUntouched подтверждает, что TLS-сервер не может занять
// порт чужого процесса. Если порт уже занят окружением, тест не трогает его.
func TestBusyPortRemainsUntouched(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:60800")
	if err != nil {
		t.Skip("60800 уже занят окружением")
	}
	defer occupied.Close()
	home := t.TempDir()
	p := UserPaths(home)
	_ = os.MkdirAll(p.Dir, 0700)
	if err = ensureCertificate(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = Serve(context.Background(), home, home); err == nil || !strings.Contains(err.Error(), "порт 60800 занят") {
		t.Fatalf("ожидалась ошибка порта: %v", err)
	}
}

// TestCertificateRenewalWriteFailurePreservesPair воспроизводит обновление
// действующей установки за 25 дней до истечения сертификата. Отказ публикации
// нового сертификата не должен ломать HTTPS и последующую попытку обновления.
func TestCertificateRenewalWriteFailurePreservesPair(t *testing.T) {
	p := UserPaths(t.TempDir())
	if err := os.MkdirAll(p.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := ensureCertificate(p, now.Add(-340*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(p.Dir, "server.pem"), filepath.Join(p.Dir, "server.key")
	originalCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	originalKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("отказ записи нового сертификата")
	err = ensureCertificateWithWriter(p, now, func(path string, data []byte, mode os.FileMode) error {
		if path == certPath {
			return writeErr
		}
		return writeFile(path, data, mode)
	})
	if !errors.Is(err, writeErr) {
		t.Fatalf("ошибка записи потеряна: %v", err)
	}
	// Сравнение файлов доказывает сохранность прежней пары, а загрузка и TLS-
	// запрос ниже проверяют пользовательский результат: HTTPS всё ещё работает.
	for path, want := range map[string][]byte{certPath: originalCert, keyPath: originalKey} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("после сбоя изменён %s: %v", filepath.Base(path), err)
		}
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(desktopHandler(http.NotFoundHandler()))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	server.StartTLS()
	defer server.Close()
	client, err := readyClient(p)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	if err = ready(context.Background(), client); err != nil {
		t.Fatalf("HTTPS после сбоя недоступен: %v", err)
	}
	// Обычный повтор без отказа выпускает новый сертификат с прежним ключом.
	if err = ensureCertificate(p, now); err != nil {
		t.Fatalf("повторная попытка: %v", err)
	}
	renewedCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	unchangedKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(renewedCert, originalCert) || !bytes.Equal(unchangedKey, originalKey) {
		t.Fatal("сертификат должен обновиться, а ключ — сохраниться")
	}
	renewedPair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := x509.ParseCertificate(renewedPair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(renewed)
	if _, err = renewed.Verify(x509.VerifyOptions{Roots: roots, DNSName: Host, CurrentTime: now}); err != nil {
		t.Fatal(err)
	}
	if !renewed.NotAfter.After(now.Add(364 * 24 * time.Hour)) {
		t.Fatal("срок сертификата не продлён")
	}
}
