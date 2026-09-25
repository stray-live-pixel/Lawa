package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const uiLockName = ".ui.lock"

// UIEndpoint — адрес общего UI и, только у создавшего его процесса, listener.
// Владелец передаёт listener в ServeRuns и вызывает Close после остановки HTTP.
// У клиента Listener равен nil: его завершение не останавливает чужой сервер.
type UIEndpoint struct {
	URL      string
	Listener net.Listener
	lock     *os.File
}

// Close освобождает только собственные ресурсы. Файл блокировки не удаляется:
// иначе два процесса могли бы захватить разные inode и запустить два сервера.
func (e *UIEndpoint) Close() error {
	var err error
	if e.Listener != nil {
		if closeErr := e.Listener.Close(); !errors.Is(closeErr, net.ErrClosed) {
			err = closeErr
		}
		e.Listener = nil
	}
	if e.lock != nil {
		err = errors.Join(err, e.lock.Close())
		e.lock = nil
	}
	return err
}

// uiIdentity подтверждает приложение и хранилище, не раскрывая путь к нему.
// Это проверка совместимости локального сервера, а не механизм авторизации.
type uiIdentity struct {
	Application string `json:"application"`
	Protocol    int    `json:"protocol"`
	RootID      string `json:"rootId"`
}

// identityForUI одинаково идентифицирует root при обращении через симлинк.
func identityForUI(root string) uiIdentity {
	absolute, err := filepath.Abs(root)
	if err != nil {
		absolute = filepath.Clean(root)
	}
	if canonical, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = canonical
	}
	digest := sha256.Sum256([]byte(absolute))
	return uiIdentity{Application: "lawa", Protocol: 1, RootID: hex.EncodeToString(digest[:])}
}

// AcquireUI находит UI для root либо резервирует порт новому серверу. flock
// удерживается весь срок жизни сервера, включая запуск HTTP: параллельный клиент
// ждёт готовности владельца, а после crash может сам захватить освобождённый lock.
// Старая запись URL без блокировки не считается живым сервером.
func AcquireUI(ctx context.Context, root string) (_ *UIEndpoint, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	file, err := openUILock(root)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			err = errors.Join(err, file.Close())
		}
	}()
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			// Совместимость с serve, который уже слушает стандартный адрес.
			standardURL := "http://" + DefaultAddress
			if matchesUI(ctx, standardURL, root) {
				return &UIEndpoint{URL: standardURL}, nil
			}
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
			if listenErr != nil {
				return nil, listenErr
			}
			endpoint := &UIEndpoint{URL: "http://" + listener.Addr().String(), Listener: listener, lock: file}
			if err = publishUI(file, endpoint.URL); err != nil {
				listener.Close()
				return nil, err
			}
			transferred = true
			return endpoint, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, err
		}
		// Владелец мог ещё не дописать адрес или не войти в Serve. Частичная
		// запись и временный отказ HTTP означают ожидание, а не второй сервер.
		data := make([]byte, 1024)
		n, _ := file.ReadAt(data, 0)
		var address string
		if json.Unmarshal(data[:n], &address) == nil && matchesUI(ctx, address, root) {
			return &UIEndpoint{URL: address}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// openUILock не следует симлинку и не принимает FIFO за служебный файл.
func openUILock(root string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(root, uiLockName), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%s должен быть обычным файлом", uiLockName)
	}
	return file, nil
}

// publishUI меняет содержимое стабильного lock-файла только под эксклюзивным
// flock. Читатели при частичной записи ждут готовности; crash оставляет lock
// свободным, поэтому атомарная замена inode здесь не нужна и была бы ошибкой.
func publishUI(file *os.File, address string) error {
	data, err := json.Marshal(address)
	if err != nil {
		return err
	}
	if err = file.Truncate(0); err != nil {
		return err
	}
	_, err = file.WriteAt(data, 0)
	return err
}

// matchesUI делает ограниченный loopback-запрос без proxy и редиректов. Занятый
// порт, другой root или несовместимое приложение не считаются готовым UI.
func matchesUI(ctx context.Context, address, root string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/api/ui", nil)
	if err != nil || req.URL.Scheme != "http" || req.URL.User != nil || !net.ParseIP(req.URL.Hostname()).IsLoopback() {
		return false
	}
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 500 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	var identity uiIdentity
	return response.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(response.Body, 1024)).Decode(&identity) == nil && identity == identityForUI(root)
}

// registerServedUI публикует явно запущенный loopback serve, включая его
// нестандартный порт. Если общий UI уже занят, явный serve продолжает работать
// самостоятельно: он мог быть запрошен для запуска Engine команд.
func registerServedUI(root string, listener net.Listener) (*os.File, error) {
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return nil, nil
	}
	file, err := openUILock(root)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, nil
		}
		return nil, err
	}
	if err = publishUI(file, "http://"+net.JoinHostPort(host, port)); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
