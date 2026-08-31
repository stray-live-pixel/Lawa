package capacity

import (
	"bufio"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSharedSlots проверяет пользовательский сценарий нескольких процессов на
// одном root: два слота выдаются разным Pool, третий ждёт, Release открывает место.
func TestSharedSlots(t *testing.T) {
	root := t.TempDir()
	first, err := Configure(root, "2")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Configure(root, "")
	if err != nil {
		t.Fatal(err)
	}
	leaseOne, ok, err := first.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("первый слот не выдан: ok=%t err=%v", ok, err)
	}
	leaseTwo, ok, err := second.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("второй слот не выдан: ok=%t err=%v", ok, err)
	}
	if lease, available, acquireErr := first.TryAcquire(); acquireErr != nil || available || lease != nil {
		t.Fatalf("третий слот ошибочно доступен: lease=%v available=%t err=%v", lease, available, acquireErr)
	}
	if err = leaseOne.Release(); err != nil {
		t.Fatal(err)
	}
	reused, ok, err := second.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("освобождённый слот не переиспользован: ok=%t err=%v", ok, err)
	}
	if err = reused.Release(); err == nil {
		err = leaseTwo.Release()
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestSlotReleasedWhenProcessExits запускает отдельный процесс, дожидается
// захвата единственного слота и аварийно завершает владельца без Release. Новый
// процесс после этого должен получить слот: гарантию восстановления даёт flock
// ядра, а не cleanup-файл, который мог бы навсегда остаться после crash.
func TestSlotReleasedWhenProcessExits(t *testing.T) {
	const helperRoot = "LAWA_TEST_CAPACITY_HELPER_ROOT"
	if root := os.Getenv(helperRoot); root != "" {
		pool, err := Configure(root, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, available, acquireErr := pool.TryAcquire(); acquireErr != nil || !available {
			t.Fatalf("helper не получил слот: available=%t err=%v", available, acquireErr)
		}
		_, _ = os.Stdout.WriteString("acquired\n")
		select {}
	}

	root := t.TempDir()
	if _, err := Configure(root, "1"); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestSlotReleasedWhenProcessExits$")
	command.Env = append(os.Environ(), helperRoot+"="+root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	scanner := bufio.NewScanner(stdout)
	confirmed := make(chan bool, 1)
	go func() { confirmed <- scanner.Scan() && scanner.Text() == "acquired" }()
	select {
	case ok := <-confirmed:
		if ok {
			break
		}
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("helper не подтвердил слот: %q, %v", scanner.Text(), scanner.Err())
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("helper не подтвердил захват слота за три секунды")
	}
	if err = command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if waitErr := command.Wait(); waitErr == nil {
		t.Fatal("аварийно завершённый helper неожиданно вернул успех")
	}
	pool, err := Configure(root, "")
	if err != nil {
		t.Fatal(err)
	}
	lease, available, err := pool.TryAcquire()
	if err != nil || !available {
		t.Fatalf("ядро не освободило слот процесса: available=%t err=%v", available, err)
	}
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestConfigurationPersistsAndValidates проверяет root-level семантику флага:
// следующий процесс читает значение с диска, а повреждённый файл не отключает
// лимит молча и не превращает его в бесконечный.
func TestConfigurationPersistsAndValidates(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	if _, err := Configure(root, "3"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, settingsFilename))
	if err != nil {
		t.Fatal(err)
	}
	var saved settings
	if err = json.Unmarshal(data, &saved); err != nil || saved.Version != 1 || saved.MaxParallel != 3 {
		t.Fatalf("настройка не сохранена: %+v, %v", saved, err)
	}
	if _, err = Configure(root, ""); err != nil {
		t.Fatalf("сохранённая настройка не прочитана: %v", err)
	}
	for _, invalid := range []string{"0", "-1", "bad"} {
		if _, err = Configure(root, invalid); err == nil {
			t.Errorf("принят неверный лимит %q", invalid)
		}
	}
	if err = os.WriteFile(filepath.Join(root, settingsFilename), []byte(`{"version":1,"maxParallel":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = Configure(root, ""); err == nil {
		t.Fatal("повреждённый лимит молча принят как отсутствие ограничения")
	}
	if _, err = Configure(root, "2"); err != nil {
		t.Fatalf("явный флаг не восстановил повреждённую настройку: %v", err)
	}
	if _, err = Configure(root, ""); err != nil {
		t.Fatalf("восстановленная настройка не читается: %v", err)
	}
}
