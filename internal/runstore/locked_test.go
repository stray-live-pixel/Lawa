//go:build darwin || linux

package runstore

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// testLockedRun создаёт изолированный run; cleanup освобождает lock даже при Fatal.
func testLockedRun(t *testing.T) (string, Snapshot, *LockedRun) {
	t.Helper()
	root := t.TempDir()
	s, err := Create(root, testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenLocked(root, s.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return root, s, r
}

// TestLockedUpdates проверяет журнал намерения, постоянство связи с чатом и
// ручные продолжения. Неверное обновление не меняет диск и не отравляет владельца.
func TestLockedUpdates(t *testing.T) {
	root, initial, r := testLockedRun(t)
	id := initial.Meta.Steps[0].ID
	memory := filepath.Join(root, initial.Meta.RunID, "memory", initial.Meta.Steps[0].ThreadID+".md")
	mustWrite(t, memory, []byte("итог агента"))
	if err := r.Update(id, scheduler.Running, "chat"); err == nil {
		t.Fatal("пропущена обязательная запись намерения")
	}
	for _, next := range []struct {
		state scheduler.State
		chat  string
	}{
		{scheduler.Starting, ""}, {scheduler.Starting, ""},
		{scheduler.Starting, "chat"}, {scheduler.Unknown, "chat"},
		{scheduler.WaitingForApproval, "chat"}, {scheduler.Cancelled, "chat"},
		{scheduler.Failed, "chat"}, {scheduler.Succeeded, "chat"}, {scheduler.Running, "chat"},
	} {
		if err := r.Update(id, next.state, next.chat); err != nil {
			t.Fatal(err)
		}
		if err := r.Update(id, scheduler.Pending, ""); err == nil {
			t.Fatal("намерение или существующий чат сброшены в Pending")
		}
		want := initial
		want.Meta.Steps = append([]Step(nil), initial.Meta.Steps...)
		want.Meta.Steps[0].State, want.Meta.Steps[0].CodexThreadID = next.state, next.chat
		if got, err := Load(root, initial.Meta.RunID); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("не сохранён полный снимок: %+v, %v", got, err)
		}
	}
	before, err := r.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []struct {
		id, chat string
		state    scheduler.State
	}{
		{id, "", scheduler.Pending}, {id, "chat", scheduler.Starting},
		{id, "other", scheduler.Running}, {id, "", scheduler.Unknown},
		{id, "chat", "invalid"}, {"missing", "chat", scheduler.Running},
	} {
		if err := r.Update(next.id, next.state, next.chat); err == nil {
			t.Fatalf("принято неверное обновление: %+v", next)
		}
	}
	if got, err := r.Load(); err != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("отказ изменил сохранённый снимок: %+v, %v", got, err)
	}
	other := initial.Meta.Steps[1].ID
	if err := r.Update(other, scheduler.Starting, ""); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []string{"chat", initial.Meta.InitiatorThreadID} {
		if err := r.Update(other, scheduler.Running, chat); err == nil {
			t.Fatalf("принят общий чат %q", chat)
		}
	}
	for _, name := range []string{"meta.json", "coordinator.lock"} {
		info, err := os.Stat(filepath.Join(root, initial.Meta.RunID, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("нарушены права %s: %v, %v", name, info, err)
		}
	}
	if data, err := os.ReadFile(memory); err != nil || string(data) != "итог агента" {
		t.Fatalf("обновление изменило память: %q, %v", data, err)
	}
}

// TestReserveWave проверяет единую публикацию параллельной волны и отказ до
// изменения снимка при неизвестном, повторном или уже запущенном шаге.
func TestReserveWave(t *testing.T) {
	_, initial, r := testLockedRun(t)
	ids := []string{initial.Meta.Steps[0].ID, initial.Meta.Steps[1].ID}
	for _, invalid := range [][]string{{ids[0], "missing"}, {ids[0], ids[0]}} {
		if err := r.Reserve(invalid); err == nil {
			t.Fatalf("принята неверная волна: %v", invalid)
		}
		got, err := r.Load()
		if err != nil || !reflect.DeepEqual(got, initial) {
			t.Fatalf("отказ изменил снимок: %+v, %v", got, err)
		}
	}
	if err := r.Reserve(ids); err != nil {
		t.Fatal(err)
	}
	got, err := r.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range got.Meta.Steps {
		want := scheduler.Pending
		if step.ID == ids[0] || step.ID == ids[1] {
			want = scheduler.Starting
		}
		if step.State != want || step.CodexThreadID != "" {
			t.Errorf("волна сохранена частично: %+v", step)
		}
	}
	if err := r.Reserve(ids[:1]); err == nil {
		t.Fatal("уже запущенный шаг зарезервирован повторно")
	}
	if err := r.Reserve(nil); err != nil {
		t.Fatalf("пустая волна должна быть безопасна: %v", err)
	}
}

// TestReleaseUnattempted проверяет единственное безопасное исключение из запрета
// Starting → Pending. Пока клиент в том же процессе подтвердил, что thread/start
// не вызывался, резервирование можно снять; известный или возможный чат защищён
// от такого сброса и повторного создания.
func TestReleaseUnattempted(t *testing.T) {
	_, initial, r := testLockedRun(t)
	id := initial.Meta.Steps[0].ID
	if err := r.ReleaseUnattempted(id); err == nil {
		t.Fatal("Pending ошибочно принят как неподтверждённое резервирование")
	}
	if err := r.Reserve([]string{id}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseUnattempted(id); err != nil {
		t.Fatal(err)
	}
	got, err := r.Load()
	if err != nil || got.Meta.Steps[0].State != scheduler.Pending || got.Meta.Steps[0].CodexThreadID != "" {
		t.Fatalf("безопасное резервирование не снято: %+v, %v", got.Meta.Steps, err)
	}
	// После подтверждённого отказа следующий явный resume снова получает право
	// зарезервировать шаг, но появление ID немедленно делает сброс недопустимым.
	if err := r.Reserve([]string{id}); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(id, scheduler.Starting, "chat-one"); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseUnattempted(id); err == nil {
		t.Fatal("резервирование с известным чатом сброшено в Pending")
	}
	got, err = r.Load()
	if err != nil || got.Meta.Steps[0].State != scheduler.Starting || got.Meta.Steps[0].CodexThreadID != "chat-one" {
		t.Fatalf("отказ сброса изменил известный чат: %+v, %v", got.Meta.Steps, err)
	}
}

// TestLockedConcurrentUpdates защищает от потери одного из параллельных
// обновлений: второй вызов должен прочитать meta только после публикации первого.
// Пауза в существующем Sync-hook фиксирует первый вызов перед Rename. Окно
// ожидания проверяет, что второй ещё не завершился; затем сверяем оба состояния.
func TestLockedConcurrentUpdates(t *testing.T) {
	root, want, r := testLockedRun(t)
	paused, release := make(chan struct{}), make(chan struct{})
	resume := sync.OnceFunc(func() { close(release) })
	defer resume() // При ошибке теста не оставляем Update и cleanup заблокированными.
	pause := sync.OnceFunc(func() { close(paused); <-release })
	done := make(chan error, 2)
	go func() {
		done <- r.update(want.Meta.Steps[0].ID, scheduler.Starting, "", func(f *os.File) error {
			pause() // Только первый Sync: второй сохраняет уже переименованный каталог.
			return f.Sync()
		})
	}()
	select {
	case <-paused:
	case err := <-done:
		t.Fatalf("первое обновление не дошло до сохранения: %v", err)
	}
	go func() { done <- r.Update(want.Meta.Steps[1].ID, scheduler.Starting, "") }()
	remaining := 2
	select {
	case err := <-done:
		remaining--
		t.Errorf("второе обновление завершилось до публикации первого: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	resume()
	for range remaining {
		if err := <-done; err != nil {
			t.Fatalf("параллельное обновление: %v", err)
		}
	}
	want.Meta.Steps[0].State, want.Meta.Steps[1].State = scheduler.Starting, scheduler.Starting
	if got, err := Load(root, want.Meta.RunID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("одно из параллельных обновлений потеряно: %+v, %v", got, err)
	}
}

// TestLockDamagedRun проверяет освобождение lock при ошибке чтения и запрет
// симлинка вместо lock-файла, даже если цель находится внутри того же run.
func TestLockDamagedRun(t *testing.T) {
	root, s, r := testLockedRun(t)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(root, s.Meta.RunID, "task.md")
	mustWrite(t, task, nil)
	if got, err := OpenLocked(root, s.Meta.RunID); err == nil || got != nil {
		t.Fatalf("принят повреждённый запуск: %v, %v", got, err)
	}
	mustWrite(t, task, []byte(s.Task))
	reopened, err := OpenLocked(root, s.Meta.RunID)
	if err != nil {
		t.Fatalf("отказ чтения оставил блокировку: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, s.Meta.RunID, "coordinator.lock")
	if err := os.Rename(lock, lock+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("coordinator.lock.saved", lock); err != nil {
		t.Fatal(err)
	}
	if got, err := OpenLocked(root, s.Meta.RunID); err == nil || got != nil {
		t.Fatalf("принят lock-симлинк: %v, %v", got, err)
	}
}

// TestUpdateSyncFailure моделирует отказ до/после Rename. Старый или новый JSON
// целиком читается, но повтор у прежнего владельца запрещён в обоих случаях.
func TestUpdateSyncFailure(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(map[bool]string{false: "файл", true: "каталог"}[afterRename], func(t *testing.T) {
			root, initial, r := testLockedRun(t)
			id := initial.Meta.Steps[0].ID
			err := r.update(id, scheduler.Starting, "", func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if info.IsDir() == afterRename {
					return syscall.EIO
				}
				return f.Sync()
			})
			if !errors.Is(err, syscall.EIO) || !errors.Is(r.Update(id, scheduler.Starting, ""), syscall.EIO) {
				t.Fatalf("не заблокирован повтор после сбоя: %v", err)
			}
			if _, err := r.Load(); !errors.Is(err, syscall.EIO) {
				t.Fatalf("неопределённый снимок доступен владельцу: %v", err)
			}
			if afterRename {
				initial.Meta.Steps[0].State = scheduler.Starting
			}
			if got, err := Load(root, initial.Meta.RunID); err != nil || !reflect.DeepEqual(got, initial) {
				t.Fatalf("потеряна целостность снимка: %+v, %v", got, err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenLocked(root, initial.Meta.RunID)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.Update(id, scheduler.Starting, ""); err != nil {
				t.Fatalf("после повторного открытия запись недоступна: %v", err)
			}
		})
	}
}

// TestRunLockProcess проверяет конкуренцию настоящих процессов и освобождение
// flock при выходе без Close. Сохраняется Starting, а не повод создать новый чат.
func TestRunLockProcess(t *testing.T) {
	const env = "LAWA_TEST_LOCK_ROOT"
	if root := os.Getenv(env); root != "" {
		r, err := OpenLocked(root, os.Getenv("LAWA_TEST_LOCK_ID"))
		if os.Getenv("LAWA_TEST_LOCK_BUSY") == "1" {
			if !errors.Is(err, ErrRunLocked) {
				t.Fatalf("второй координатор не отвергнут: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		s, err := r.Load()
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Update(s.Meta.Steps[0].ID, scheduler.Starting, ""); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // Намеренно пропускаем Close и defer: дескриптор закрывает ОС.
	}
	root, s, r := testLockedRun(t)
	for _, busy := range []string{"1", "0"} {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRunLockProcess$")
		cmd.Env = append(os.Environ(), env+"="+root, "LAWA_TEST_LOCK_ID="+s.Meta.RunID, "LAWA_TEST_LOCK_BUSY="+busy)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("дочерний координатор: %v\n%s", err, output)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Update(s.Meta.Steps[0].ID, scheduler.Starting, ""); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("закрытый владелец сохранил право записи: %v", err)
	}
	reopened, err := OpenLocked(root, s.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Load()
	if err != nil || got.Meta.Steps[0].State != scheduler.Starting {
		t.Fatalf("после выхода потеряно намерение: %+v, %v", got, err)
	}
}

// TestUpdateWriteFailure получает настоящий EFBIG от Write. Лимит действует
// только в дочернем процессе; прежняя meta и память не удаляются как новый run.
func TestUpdateWriteFailure(t *testing.T) {
	const env = "LAWA_TEST_UPDATE_WRITE_FAILURE"
	if os.Getenv(env) != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUpdateWriteFailure$")
		cmd.Env = append(os.Environ(), env+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("проверка отказа записи: %v\n%s", err, output)
		}
		return
	}
	root, initial, r := testLockedRun(t)
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	limited := original
	limited.Cur = 1
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Fatal(err)
	}
	err := r.Update(initial.Meta.Steps[0].ID, scheduler.Starting, "")
	if restoreErr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("ожидался отказ Write: %v", err)
	}
	if got, err := Load(root, initial.Meta.RunID); err != nil || !reflect.DeepEqual(got, initial) {
		t.Fatalf("не сохранён прежний run: %+v, %v", got, err)
	}
}
