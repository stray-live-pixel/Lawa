//go:build darwin || linux

// Package capacity ограничивает суммарное число активных App Server turn для
// одного root Lawa. Отдельные процессы договариваются через flock на стабильных
// slot-файлах; завершение процесса освобождает занятые им слоты силами ядра.
package capacity

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	settingsFilename = "parallel.json"
	settingsLockName = "parallel.lock"
)

// settings — небольшой root-level контракт. Отсутствующий файл сохраняет
// совместимость прежних root: без явного --max-parallel лимит не применяется.
type settings struct {
	Version     int `json:"version"`
	MaxParallel int `json:"maxParallel"`
}

// Pool перечитывает настройку перед каждым захватом. Поэтому новое значение
// применяется и к уже работающим coordinator при следующем запуске кубика.
// Нулевой root используется только внутренним Unlimited и не обращается к диску.
type Pool struct{ root string }

// Lease удерживает один межпроцессный слот до Release. Значение нельзя копировать:
// повторный Release безопасен, но только исходный указатель владеет дескриптором.
type Lease struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	released bool
}

var (
	heldMu sync.Mutex
	held   = map[string]bool{}
)

// Unlimited возвращает пул без файлового лимита. Он нужен для старых внутренних
// вызовов coordinator и root, где пользователь ещё не задавал maxParallel.
func Unlimited() *Pool { return &Pool{} }

// Validate проверяет явное значение CLI без чтения или изменения root. Пустая
// строка означает отсутствие флага и поэтому допустима: фактический лимит тогда
// будет прочитан Configure из сохранённой настройки.
func Validate(requested string) error {
	if requested == "" {
		return nil
	}
	maximum, err := strconv.Atoi(requested)
	if err != nil || maximum <= 0 {
		return errors.New("--max-parallel должен быть положительным целым числом")
	}
	return nil
}

// Configure читает сохранённый лимит или атомарно меняет его по явному флагу.
// Пустое requested ничего не создаёт и использует ранее сохранённую настройку.
// Непустое значение обязано быть положительным целым числом.
func Configure(root, requested string) (result *Pool, err error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("лимит параллельности требует root")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("определить root лимита параллельности: %w", err)
	}
	pool := &Pool{root: absolute}
	if requested == "" {
		if _, err = pool.limit(); err != nil {
			return nil, err
		}
		return pool, nil
	}
	if err = Validate(requested); err != nil {
		return nil, err
	}
	maximum, _ := strconv.Atoi(requested)
	if err = os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("создать root лимита параллельности: %w", err)
	}
	lock, err := openRegular(filepath.Join(absolute, settingsLockName), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("открыть блокировку настройки параллельности: %w", err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, errors.Join(fmt.Errorf("заблокировать настройку параллельности: %w", err), lock.Close())
	}
	defer func() {
		err = errors.Join(err, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN), lock.Close())
	}()
	// Явный флаг всегда публикует целую новую настройку. Это не только упрощает
	// конкурентное обновление, но и позволяет пользователю восстановить root,
	// если прежний parallel.json оборвался или был повреждён вручную.
	if err = saveSettings(absolute, settings{Version: 1, MaxParallel: maximum}); err != nil {
		return nil, err
	}
	return pool, nil
}

// TryAcquire получает любой свободный слот текущего лимита. false означает
// нормальное ожидание: coordinator не резервирует шаг и повторяет попытку позже.
func (p *Pool) TryAcquire() (*Lease, bool, error) {
	if p == nil || p.root == "" {
		return &Lease{}, true, nil
	}
	maximum, err := p.limit()
	if err != nil {
		return nil, false, err
	}
	if maximum == 0 {
		return &Lease{}, true, nil
	}
	for slot := 0; slot < maximum; slot++ {
		path := filepath.Join(p.root, fmt.Sprintf(".parallel-slot-%06d.lock", slot))
		if !reserveLocal(path) {
			continue
		}
		file, busy, openErr := tryLockSlot(path)
		if openErr != nil {
			releaseLocal(path)
			return nil, false, openErr
		}
		if busy {
			releaseLocal(path)
			continue
		}
		return &Lease{file: file, path: path}, true, nil
	}
	return nil, false, nil
}

// Release освобождает слот для другого workflow. Close снимает flock даже при
// ошибке возврата; локальная карта очищается всегда, чтобы не создать вечную
// блокировку внутри долгоживущего тестового или встраивающего процесса.
func (l *Lease) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	var err error
	if l.file != nil {
		err = l.file.Close()
	}
	if l.path != "" {
		releaseLocal(l.path)
	}
	return err
}

func (p *Pool) limit() (int, error) {
	configured, exists, err := readSettings(p.root)
	if err != nil || !exists {
		return 0, err
	}
	return configured.MaxParallel, nil
}

func readSettings(root string) (settings, bool, error) {
	path := filepath.Join(root, settingsFilename)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return settings{}, false, nil
	}
	if err != nil {
		return settings{}, false, fmt.Errorf("прочитать настройку параллельности: %w", err)
	}
	if !info.Mode().IsRegular() {
		return settings{}, false, fmt.Errorf("%s должен быть обычным файлом", settingsFilename)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return settings{}, false, fmt.Errorf("прочитать настройку параллельности: %w", err)
	}
	var configured settings
	if err = json.Unmarshal(data, &configured, json.RejectUnknownMembers(true)); err != nil || configured.Version != 1 || configured.MaxParallel <= 0 {
		if err == nil {
			err = errors.New("неподдерживаемая версия или неположительный maxParallel")
		}
		return settings{}, false, fmt.Errorf("повреждён %s: %w", settingsFilename, err)
	}
	return configured, true, nil
}

func saveSettings(root string, configured settings) error {
	data, err := json.Marshal(configured)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(root, ".parallel-settings-*.tmp")
	if err != nil {
		return fmt.Errorf("создать временную настройку параллельности: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err = temporary.Write(data); err == nil {
		err = temporary.Sync()
	}
	if err = errors.Join(err, temporary.Close()); err != nil {
		return fmt.Errorf("сохранить настройку параллельности: %w", err)
	}
	if err = os.Rename(temporaryPath, filepath.Join(root, settingsFilename)); err != nil {
		return fmt.Errorf("опубликовать настройку параллельности: %w", err)
	}
	directory, err := os.Open(root)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func tryLockSlot(path string) (*os.File, bool, error) {
	file, err := openRegular(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, false, fmt.Errorf("открыть слот параллельности: %w", err)
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, true, closeErr
		}
		return nil, false, errors.Join(fmt.Errorf("заблокировать слот параллельности: %w", err), closeErr)
	}
	return file, false, nil
}

func openRegular(path string, flags int) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("служебный файл лимита должен быть обычным файлом")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("служебный файл лимита должен быть обычным файлом")
		}
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func reserveLocal(path string) bool {
	heldMu.Lock()
	defer heldMu.Unlock()
	if held[path] {
		return false
	}
	held[path] = true
	return true
}

func releaseLocal(path string) {
	heldMu.Lock()
	delete(held, path)
	heldMu.Unlock()
}
