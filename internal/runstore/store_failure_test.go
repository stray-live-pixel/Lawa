//go:build darwin || linux

package runstore

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// TestCreateWriteFailure вызывает настоящий отказ Write после создания папки
// run и начала записи task.md. Лимит размера файла действует только в дочернем
// процессе: тест не заполняет диск и не меняет ограничения остальных тестов.
// RLIMIT_FSIZE и SIGXFSZ доступны на целевых macOS/Linux, поэтому тест отделён
// от переносимых проверок. Это модель отказа записи, а не поломка диска.
func TestCreateWriteFailure(t *testing.T) {
	const childEnv = "LAWA_TEST_CREATE_WRITE_FAILURE"
	if os.Getenv(childEnv) != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCreateWriteFailure$", "-test.v")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("проверка отказа записи в дочернем процессе: %v\n%s", err, output)
		}
		return
	}
	root, in := t.TempDir(), testInput(t)
	old, err := Create(root, in)
	if err != nil {
		t.Fatal(err)
	}
	memory := filepath.Join(root, old.Meta.RunID, "memory", old.Meta.Steps[0].ThreadID+".md")
	mustWrite(t, memory, []byte("сохранить память агента"))
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	limited := original
	limited.Cur = 4096
	// Игнорируем сигнал, чтобы ОС вернула EFBIG, а не завершила процесс до
	// выполнения очистки. Аварийная остановка имеет другой контракт Create.
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
			t.Errorf("восстановление лимита: %v", err)
		}
		signal.Reset(syscall.SIGXFSZ)
	})
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Fatal(err)
	}
	in.Task = strings.Repeat("x", 8192)
	got, err := Create(root, in)
	if restoreErr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if !errors.Is(err, syscall.EFBIG) || !reflect.DeepEqual(got, Snapshot{}) {
		t.Fatalf("ожидались отказ записи и пустой снимок: %+v, %v", got, err)
	}
	var writeErr *os.PathError
	if !errors.As(err, &writeErr) || writeErr.Op != "write" || filepath.Base(writeErr.Path) != "task.md" {
		t.Fatalf("отказ должен произойти при записи нового файла: %v", err)
	}
	var failure *CreateError
	if !errors.As(err, &failure) || failure.RunDir != filepath.Dir(writeErr.Path) || failure.CleanupErr != nil {
		t.Fatalf("обработчику недоступны путь и результат очистки: %v", err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 1 || entries[0].Name() != old.Meta.RunID {
		t.Fatalf("не удалён новый запуск либо потерян старый: %v, %v", entries, readErr)
	}
	if restored, err := Load(root, old.Meta.RunID); err != nil || !reflect.DeepEqual(restored, old) {
		t.Fatalf("старый запуск изменён: %+v, %v", restored, err)
	}
	if data, err := os.ReadFile(memory); err != nil || string(data) != "сохранить память агента" {
		t.Fatalf("старая память изменена: %q, %v", data, err)
	}
	for _, detail := range []string{"Превышен допустимый размер файла", "Незавершённая папка удалена", "повторите создание запуска", writeErr.Path} {
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("в ошибке нет понятного контекста %q: %v", detail, err)
		}
	}
	// После устранения причины тот же вход сохраняется без ручной очистки.
	if retried, err := Create(root, in); err != nil {
		t.Fatalf("повтор после снятия лимита: %v", err)
	} else if loaded, err := Load(root, retried.Meta.RunID); err != nil || !reflect.DeepEqual(loaded, retried) {
		t.Fatalf("повторный запуск не читается: %+v, %v", loaded, err)
	}
}

// TestCreateCleanupFailure проверяет двойной сбой: файлы уже записаны, Sync
// отказал, а потеря права записи в новый run мешает RemoveAll удалить его файлы.
// Ошибка должна сохранить обе причины и не утверждать, что очистка состоялась.
// root обходит обычные права Unix, поэтому для него этот сценарий неприменим.
func TestCreateCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root может удалять файлы без права записи в папку")
	}
	root, in := t.TempDir(), testInput(t)
	old, err := Create(root, in)
	if err != nil {
		t.Fatal(err)
	}
	var runDir string
	got, err := create(root, in, func(path string) error {
		if filepath.Base(path) != "memory" {
			return syncDir(path)
		}
		runDir = filepath.Dir(path)
		// Возвращаем права перед удалением временных папок самим testing.
		t.Cleanup(func() {
			if err := os.Chmod(runDir, 0o700); err != nil {
				t.Errorf("восстановление прав: %v", err)
			}
		})
		if err := os.Chmod(runDir, 0o500); err != nil {
			t.Fatal(err)
		}
		return &os.PathError{Op: "sync", Path: path, Err: syscall.EIO}
	})
	var failure *CreateError
	if !reflect.DeepEqual(got, Snapshot{}) || !errors.As(err, &failure) || failure.RunDir != runDir {
		t.Fatalf("ожидались пустой снимок и ошибка нового запуска: %+v, %v", got, err)
	}
	if !errors.Is(failure.Cause, syscall.EIO) || !errors.Is(failure.CleanupErr, os.ErrPermission) ||
		!errors.Is(err, syscall.EIO) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("потеряна причина сохранения или очистки: %v", err)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("ожидалась оставшаяся незавершённая папка: %v", err)
	}
	message := err.Error()
	for _, detail := range []string{"Не удалось удалить незавершённую папку", "Нет прав на операцию", "удалите только эту папку", runDir, failure.CleanupErr.Error()} {
		if !strings.Contains(message, detail) {
			t.Errorf("нет контекста для устранения двойного сбоя %q: %v", detail, err)
		}
	}
	if strings.Contains(message, "Незавершённая папка удалена") {
		t.Fatalf("ошибка ложно обещает успешную очистку: %v", err)
	}
	if restored, err := Load(root, old.Meta.RunID); err != nil || !reflect.DeepEqual(restored, old) {
		t.Fatalf("двойной сбой повредил старый запуск: %+v, %v", restored, err)
	}
}
