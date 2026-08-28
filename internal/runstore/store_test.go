package runstore

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stray-live-pixel/flows-2/internal/scheduler"
)

// testInput использует продуктовый граф, но с ID, похожим на путь: он должен
// остаться ключом зависимости и никогда не стать именем файла памяти.
func testInput(t *testing.T) Input {
	t.Helper()
	data, err := os.ReadFile("../../examples/review.json")
	if err != nil {
		t.Fatal(err)
	}
	return Input{WorkflowJSON: bytes.ReplaceAll(data, []byte(`"metrics"`), []byte(`"../metrics"`)), Task: "  Проверить проект\n", Comment: "Не менять код", CWD: t.TempDir(), InitiatorThreadID: "initiator"}
}

// mustWrite имитирует изменение файла агентом или повреждение сохранённого run.
func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCreateLoad проверяет полный набор приватных файлов, точную копию входа,
// отдельные ID новых запусков и сохранность памяти при повторном чтении.
func TestCreateLoad(t *testing.T) {
	root, in := filepath.Join(t.TempDir(), "runs"), testInput(t)
	original := bytes.Clone(in.WorkflowJSON)
	s, err := Create(root, in)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, s.Meta.RunID)
	if got, err := Load(root, s.Meta.RunID); err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("снимок после чтения: %+v, %v", got, err)
	}
	if s.Task != "# Постановка задачи\n\n"+in.Task+"\n\n# Комментарий пользователя\n\n"+in.Comment+"\n" {
		t.Fatalf("постановка или комментарий изменились: %q", s.Task)
	}
	other, err := Create(root, in)
	if err != nil || other.Meta.RunID == s.Meta.RunID {
		t.Fatalf("новый запуск не получил отдельный ID: %+v, %v", other, err)
	}
	clear(in.WorkflowJSON)
	if data, err := os.ReadFile(filepath.Join(dir, "workflow.json")); err != nil || !bytes.Equal(data, original) {
		t.Fatalf("потеряна копия исходного JSON: %q, %v", data, err)
	}
	for i, step := range s.Meta.Steps {
		if step.State != scheduler.Pending || step.CodexThreadID != "" || step.ThreadID == other.Meta.Steps[i].ThreadID {
			t.Fatalf("неверное начальное состояние или повторный ID: %+v", step)
		}
		path := filepath.Join(dir, "memory", step.ThreadID+".md")
		if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
			t.Fatalf("память не создана пустой: %q, %v", data, err)
		}
		mustWrite(t, path, []byte("итог агента"))
		if _, err := Load(root, s.Meta.RunID); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "итог агента" {
			t.Fatalf("чтение изменило память: %q, %v", data, err)
		}
	}
	// Проверяем и каталоги: приватные файлы не должны лежать в общей папке run.
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if entry.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want || strings.HasSuffix(path, ".tmp") {
			t.Errorf("права или незавершённый файл %s: %v", path, info.Mode())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestInvalidInputHasNoSideEffects проверяет отказ до создания папки хранения.
func TestInvalidInputHasNoSideEffects(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"workflow":   func(in *Input) { in.WorkflowJSON = []byte(`{"id":"x","steps":[]}`) },
		"постановка": func(in *Input) { in.Task = " \n" },
		"Unicode":    func(in *Input) { in.Comment = "\xff" },
		"инициатор":  func(in *Input) { in.InitiatorThreadID = "" },
		"cwd":        func(in *Input) { in.CWD = "" },
		"нет cwd":    func(in *Input) { in.CWD = filepath.Join(in.CWD, "missing") },
		"cwd-файл":   func(in *Input) { in.CWD = "../../examples/review.json" },
	} {
		t.Run(name, func(t *testing.T) {
			root, in := filepath.Join(t.TempDir(), "not-created"), testInput(t)
			mutate(&in)
			if got, err := Create(root, in); err == nil || !reflect.DeepEqual(got, Snapshot{}) {
				t.Fatalf("принят неверный ввод: %+v, %v", got, err)
			}
			if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("проверка создала папку: %v", err)
			}
		})
	}
}

// TestMetadataStates сохраняет реальные состояния, включая неопределённый
// Starting, и отвергает повреждения, способные повторно запустить чужой чат.
func TestMetadataStates(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, m Metadata, wantError bool) {
		t.Helper()
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(root, s.Meta.RunID, "meta.json"), data)
		got, err := Load(root, s.Meta.RunID)
		if (err != nil) != wantError || wantError && !reflect.DeepEqual(got, Snapshot{}) || !wantError && !reflect.DeepEqual(got.Meta, m) {
			t.Fatalf("чтение метаданных: %+v, %v; ожидалась ошибка: %v", got.Meta, err, wantError)
		}
	}
	for _, state := range []scheduler.State{scheduler.Pending, scheduler.Starting, scheduler.Unknown, scheduler.Running, scheduler.WaitingForApproval, scheduler.Failed, scheduler.Cancelled, scheduler.Succeeded} {
		m := s.Meta
		m.Steps = slices.Clone(m.Steps)
		m.Steps[0].State = state
		if state != scheduler.Pending && state != scheduler.Starting {
			m.Steps[0].CodexThreadID = "existing-chat"
		}
		check(t, m, false)
	}
	for name, mutate := range map[string]func(*Metadata){
		"версия":                func(m *Metadata) { m.Version++ },
		"runId":                 func(m *Metadata) { m.RunID = newID() },
		"cwd":                   func(m *Metadata) { m.CWD = "relative" },
		"нулевой байт cwd":      func(m *Metadata) { m.CWD += "\x00" },
		"инициатор":             func(m *Metadata) { m.InitiatorThreadID = "" },
		"пропуск шага":          func(m *Metadata) { m.Steps = m.Steps[1:] },
		"подмена шага":          func(m *Metadata) { m.Steps[0].ID = "missing" },
		"дубликат шага":         func(m *Metadata) { m.Steps[0].ID = m.Steps[1].ID },
		"дубликат памяти":       func(m *Metadata) { m.Steps[0].ThreadID = m.Steps[1].ThreadID },
		"путь в памяти":         func(m *Metadata) { m.Steps[0].ThreadID = "../outside" },
		"нет состояния":         func(m *Metadata) { m.Steps[0].State = "" },
		"неизвестное состояние": func(m *Metadata) { m.Steps[0].State = "idle" },
		"нет чата":              func(m *Metadata) { m.Steps[0].State = scheduler.Succeeded },
		"Pending с чатом":       func(m *Metadata) { m.Steps[0].CodexThreadID = "chat" },
		"дубликат чата": func(m *Metadata) {
			for i := range 2 {
				m.Steps[i].State, m.Steps[i].CodexThreadID = scheduler.Running, "same-chat"
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := s.Meta
			m.Steps = slices.Clone(m.Steps)
			mutate(&m)
			check(t, m, true)
		})
	}
}

// TestBrokenFilesNotRecreated защищает от автоматического «лечения» run: даже
// при потере одного файла возвращается пустой результат, а память не создаётся.
func TestBrokenFilesNotRecreated(t *testing.T) {
	for _, name := range []string{"workflow.json", "task.md", "meta.json", "memory"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s, err := Create(root, testInput(t))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, s.Meta.RunID, name)
			backup := path + ".saved"
			if err := os.Rename(path, backup); err != nil {
				t.Fatal(err)
			}
			// Сначала отсутствие, затем повреждённый файл, затем абсолютная ссылка.
			for i, damage := range []func() error{
				func() error { return nil },
				func() error { return os.WriteFile(path, []byte{0xff}, 0o600) },
				func() error { return os.Symlink(backup, path) },
			} {
				if err := damage(); err != nil {
					t.Fatal(err)
				}
				if got, err := Load(root, s.Meta.RunID); err == nil || !reflect.DeepEqual(got, Snapshot{}) {
					t.Fatalf("повреждение принято: %+v, %v", got, err)
				}
				if _, err := os.Lstat(path); i == 0 && !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("отсутствующий файл был восстановлен: %v", err)
				}
				if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestStorageBoundaries проверяет отказ записи без потери старого файла и
// запрещает чтение произвольного пути вместо ID, в том числе через симлинк run.
func TestStorageBoundaries(t *testing.T) {
	root, in := t.TempDir(), testInput(t)
	file := filepath.Join(root, "existing")
	mustWrite(t, file, []byte("сохранить"))
	if err := writeNewFile(file, []byte("замена")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("разрешена перезапись: %v", err)
	}
	if _, err := Create(file, in); err == nil {
		t.Fatal("принят файл вместо папки хранения")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "сохранить" {
		t.Fatalf("ошибка записи повредила старый файл: %q, %v", data, err)
	}
	// Снаружи лежит полностью корректный run: отказ должен быть вызван границей
	// хранилища, а не случайным отсутствием метаданных в целевой папке ссылки.
	outside := t.TempDir()
	external, err := Create(outside, in)
	if err != nil {
		t.Fatal(err)
	}
	linkID := external.Meta.RunID
	if err := os.Symlink(filepath.Join(outside, linkID), filepath.Join(root, linkID)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "..", "../existing", in.CWD, strings.Repeat("a", 32) + "/..", newID(), linkID} {
		if got, err := Load(root, id); err == nil || !reflect.DeepEqual(got, Snapshot{}) {
			t.Fatalf("принят неизвестный run или выход из хранилища %q: %+v, %v", id, got, err)
		}
	}
}
