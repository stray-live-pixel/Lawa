package dashboard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestAcquireUIConcurrent имитирует одновременный старт нескольких CLI. Даже
// между захватом порта и готовностью HTTP только один участник становится сервером.
func TestAcquireUIConcurrent(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		endpoint *UIEndpoint
		err      error
	}
	const count = 8
	results := make(chan result, count)
	var servers sync.WaitGroup
	for range count {
		go func() {
			endpoint, err := AcquireUI(ctx, root)
			if err == nil && endpoint.Listener != nil {
				servers.Add(1)
				go func() {
					defer servers.Done()
					defer endpoint.Close()
					_ = ServeRuns(ctx, endpoint.Listener, root)
				}()
			}
			results <- result{endpoint, err}
		}()
	}
	owners := 0
	address := ""
	for range count {
		got := <-results
		if got.err != nil {
			t.Errorf("получить UI: %v", got.err)
			continue
		}
		if got.endpoint.Listener != nil {
			owners++
		} else if err := got.endpoint.Close(); err != nil {
			t.Error(err)
		}
		if address != "" && address != got.endpoint.URL {
			t.Errorf("созданы разные серверы: %s и %s", address, got.endpoint.URL)
		}
		address = got.endpoint.URL
	}
	if owners != 1 {
		t.Errorf("владельцев UI: %d вместо 1", owners)
	}
	if !matchesUI(ctx, address, root) {
		t.Error("завершение клиентов остановило общий UI")
	}
	cancel()
	servers.Wait()
}

// TestAcquireUIAfterOwnerExit не доверяет устаревшему URL после освобождения
// блокировки: новый процесс должен получить собственный listener.
func TestAcquireUIAfterOwnerExit(t *testing.T) {
	root := t.TempDir()
	first, err := AcquireUI(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireUI(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Listener == nil {
		t.Fatal("устаревшая запись принята за живой UI")
	}
}

// TestUIIdentity проверяет приложение, root, алиас через симлинк и запрет
// редиректов. Занятый порт сам по себе не является признаком готовности Lawa.
func TestUIIdentity(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(Handler(root))
	defer server.Close()
	alias := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if !matchesUI(t.Context(), server.URL, root) || !matchesUI(t.Context(), server.URL, alias) {
		t.Fatal("собственный UI не опознан")
	}
	if matchesUI(t.Context(), server.URL, t.TempDir()) {
		t.Fatal("принято другое хранилище")
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"application":"other"}`)) }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/api/ui", http.StatusFound)
	}))
	defer redirect.Close()
	for _, address := range []string{other.URL, redirect.URL, "http://example.invalid", "://invalid"} {
		if matchesUI(t.Context(), address, root) {
			t.Errorf("принят посторонний адрес: %s", address)
		}
	}
}

// TestAcquireUIWaitsForOwner проверяет отмену при неготовом владельце: новый
// процесс не открывает второй сервер и не снимает чужую блокировку.
func TestAcquireUIWaitsForOwner(t *testing.T) {
	root := t.TempDir()
	owner, err := AcquireUI(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if endpoint, err := AcquireUI(ctx, root); !errors.Is(err, context.DeadlineExceeded) || endpoint != nil {
		t.Fatalf("создан дубль до готовности владельца: %+v, %v", endpoint, err)
	}
}

// TestAcquireUIReusesServe обнаруживает явно запущенный serve с нестандартным
// портом. В пустом root Engine не имеет заказов и не обращается к моделям.
func TestAcquireUIReusesServe(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	registered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeWithStartup(ctx, root, "127.0.0.1:0", func() error { close(registered); return nil })
	}()
	select {
	case <-registered:
	case err := <-done:
		t.Fatalf("serve не запущен: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve не зарегистрирован")
	}
	endpoint, err := AcquireUI(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Listener != nil {
		endpoint.Close()
		t.Fatal("вместо serve создан новый сервер")
	}
	endpoint.Close()
	if !matchesUI(ctx, endpoint.URL, root) {
		t.Fatal("клиент остановил serve")
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	address := endpoint.URL[len("http://"):]
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("serve не освободил порт: %v", err)
	}
	listener.Close()
}
