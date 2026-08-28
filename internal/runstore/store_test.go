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
	"syscall"
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

// TestCreateErrorDiagnostics проверяет, что обёрнутая ошибка ОС остаётся
// распознаваемой для кода, а человек/агент получает соответствующую подсказку.
// Это проверка сообщений для заданных кодов, не воспроизведение поломок диска.
func TestCreateErrorDiagnostics(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "new-run")
	for _, tc := range []struct {
		name           string
		cause          error
		reason, action string
	}{
		{"место", syscall.ENOSPC, "нет места", "Проверьте свободное место"},
		{"квота", syscall.EDQUOT, "Исчерпана дисковая квота", "запросите её увеличение"},
		{"размер", syscall.EFBIG, "Превышен допустимый размер файла", "Проверьте размер входных данных"},
		{"права", syscall.EACCES, "Нет прав на операцию", "запросите доступ"},
		{"только чтение", syscall.EROFS, "только для чтения", "Проверьте подключение диска"},
		{"ввод-вывод", syscall.EIO, "Ошибка ввода-вывода", "не повторяйте запись"},
		{"неизвестная", errors.New("необычный отказ устройства"), "Причина требует диагностики", "Проверьте указанную операцию"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := &os.PathError{Op: "write", Path: filepath.Join(runDir, "task.md"), Err: tc.cause}
			err := &CreateError{RunDir: runDir, Cause: cause}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("обёртка скрыла системную причину: %v", err)
			}
			for _, detail := range []string{tc.reason, tc.action, cause.Error()} {
				if !strings.Contains(err.Error(), detail) {
					t.Errorf("нет причины, подсказки или исходной операции %q: %v", detail, err)
				}
			}
		})
	}
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

// TestStorageParentSync проверяет порядок сохранения имён каталогов: прежде чем
// создавать дочернюю папку, имя родителя должно быть сохранено в его родителе.
// Это проверка протокола Sync на реальных каталогах, а не имитация потери питания.
func TestStorageParentSync(t *testing.T) {
	for _, tc := range []struct {
		name     string
		depth    int
		relative bool
	}{
		{"существующий", 0, false},
		{"новый относительный", 1, true},
		{"вложенный", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := base
			want := []string{filepath.Dir(base)}
			for range tc.depth {
				want = append(want, root)
				root = filepath.Join(root, "nested")
			}
			inputRoot := root
			if tc.relative {
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				inputRoot, err = filepath.Rel(cwd, root)
				if err != nil {
					t.Fatal(err)
				}
			}
			var synced []string
			s, err := create(inputRoot, testInput(t), func(path string) error {
				synced = append(synced, path)
				return syncDir(path)
			})
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, s.Meta.RunID)
			want = append(want, filepath.Join(dir, "memory"), dir, root)
			if !slices.Equal(synced, want) {
				t.Fatalf("порядок Sync: %v; нужен: %v", synced, want)
			}
			if got, err := Load(root, s.Meta.RunID); err != nil || !reflect.DeepEqual(got, s) {
				t.Fatalf("сохранённый запуск не читается: %+v, %v", got, err)
			}
		})
	}
}

// TestStorageParentSyncFailure не позволяет начать run после отказа сохранения
// имени папки. Повтор обязан сохранить оставшуюся папку, даже если она уже есть:
// само существование после ошибки не доказывает, что её имя попало на диск.
func TestStorageParentSyncFailure(t *testing.T) {
	for _, failAt := range []string{"base", "nested"} {
		t.Run(failAt, func(t *testing.T) {
			base, in := t.TempDir(), testInput(t)
			parent := filepath.Join(base, "nested")
			root := filepath.Join(parent, "runs")
			failedDir := base
			if failAt == "nested" {
				failedDir = parent
			}
			failure := errors.New("отказ Sync родителя")
			attempts := 0
			syncParent := func(path string) error {
				if path == failedDir {
					attempts++
					return failure
				}
				return syncDir(path)
			}
			// Дважды отказываем в одном месте: наличие оставшейся папки не должно
			// позволить следующему Create обойти несостоявшуюся синхронизацию.
			for range 2 {
				got, err := create(root, in, syncParent)
				if !errors.Is(err, failure) || !reflect.DeepEqual(got, Snapshot{}) {
					t.Fatalf("ожидались ошибка Sync и пустой снимок: %+v, %v", got, err)
				}
				entries, err := os.ReadDir(root)
				if err != nil && !errors.Is(err, fs.ErrNotExist) || len(entries) != 0 {
					t.Fatalf("после отказа уже создан run: %v, %v", entries, err)
				}
			}
			if attempts != 2 {
				t.Fatalf("родитель синхронизировался %d раз вместо двух", attempts)
			}
			// После устранения ошибки обычный публичный API должен создать run
			// в той же папке без ручной очистки промежуточных каталогов.
			if _, err := Create(root, in); err != nil {
				t.Fatalf("повтор после устранения ошибки: %v", err)
			}
		})
	}
}

// TestCreateFinalSyncFailure проверяет отказ уже после публикации meta.json:
// наличие метаданных ещё не разрешает считать Create успешным и запускать агентов.
// Отдельно отказываем в Sync папки run и общего root. Новый run должен удалиться,
// а старый запуск с памятью — остаться без изменений. Подмена ошибки использует
// реальные временные файлы, но не имитирует отключение питания или поломку диска.
func TestCreateFinalSyncFailure(t *testing.T) {
	for _, failAt := range []string{"run", "root"} {
		t.Run(failAt, func(t *testing.T) {
			root, in := t.TempDir(), testInput(t)
			old, err := Create(root, in)
			if err != nil {
				t.Fatal(err)
			}
			memory := filepath.Join(root, old.Meta.RunID, "memory", old.Meta.Steps[0].ThreadID+".md")
			mustWrite(t, memory, []byte("сохранить память агента"))
			syncErr := errors.New("отказ финального Sync")
			var runDir string
			got, err := create(root, in, func(path string) error {
				// ID нового run создаётся внутри Create. Узнаём его папку при Sync
				// памяти, но сам отказ откладываем до последующего Sync run/root.
				if filepath.Base(path) == "memory" {
					runDir = filepath.Dir(path)
				}
				failedDir := runDir
				if failAt == "root" {
					failedDir = root
				}
				if runDir == "" || path != failedDir {
					return syncDir(path)
				}
				// Отказ до Rename уже проверяется другими тестами. Здесь наличие
				// meta.json обязательно, чтобы не потерять именно позднюю очистку.
				if _, err := os.Stat(filepath.Join(runDir, "meta.json")); err != nil {
					t.Fatalf("финальный Sync вызван до публикации meta.json: %v", err)
				}
				return &os.PathError{Op: "sync", Path: path, Err: syncErr}
			})
			var failure *CreateError
			if !errors.As(err, &failure) || !errors.Is(failure.Cause, syncErr) || !reflect.DeepEqual(got, Snapshot{}) {
				t.Fatalf("ожидались пустой снимок и CreateError с причиной Sync: %v", err)
			}
			if failure.RunDir != runDir || failure.CleanupErr != nil {
				t.Fatalf("неверный путь нового запуска или ошибка очистки: %v", err)
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
		})
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
	check := func(t *testing.T, m Metadata, wantError bool) error {
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
		return err
	}
	for _, state := range []scheduler.State{scheduler.Pending, scheduler.Starting, scheduler.Unknown, scheduler.Running, scheduler.WaitingForApproval, scheduler.Failed, scheduler.Cancelled, scheduler.Succeeded} {
		m := s.Meta
		m.Steps = slices.Clone(m.Steps)
		m.Steps[0].State = state
		if state != scheduler.Pending && state != scheduler.Starting {
			m.Steps[0].CodexThreadID = "existing-chat"
		}
		check(t, m, false)
		if state == scheduler.Starting {
			m.Steps[0].CodexThreadID = "existing-chat"
			check(t, m, false)
		}
		// Намеренно создаём теоретический крайний случай: корректный код записи
		// не подставляет чат управления вместо исполнителя. Это не воспроизведение
		// реального инцидента. Отдельная ошибка нужна при любом состоянии: даже
		// для Pending совпадение с инициатором точнее общей ошибки непустого ID.
		m.Steps[0].CodexThreadID = m.InitiatorThreadID
		t.Run("чат инициатора вместо исполнителя/"+string(state), func(t *testing.T) {
			err := check(t, m, true)
			if !errors.Is(err, ErrInitiatorAsExecutor) {
				t.Fatalf("обработчик не сможет распознать конфликт чатов: %v", err)
			}
			if !strings.Contains(err.Error(), m.RunID) || !strings.Contains(err.Error(), m.Steps[0].ID) {
				t.Fatalf("в ошибке не хватает запуска или шага для диагностики: %v", err)
			}
		})
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
			damages := []func() error{
				func() error { return nil },
				func() error { return os.WriteFile(path, []byte{0xff}, 0o600) },
				func() error { return os.Symlink(backup, path) },
			}
			// Пустая или пробельная постановка тоже непригодна для продолжения.
			// Во всех случаях ошибка должна указывать на task.md, а не meta.json.
			if name == "task.md" {
				damages = append(damages,
					func() error { return os.WriteFile(path, nil, 0o600) },
					func() error { return os.WriteFile(path, []byte(" \n\t"), 0o600) },
				)
			}
			for i, damage := range damages {
				if err := damage(); err != nil {
					t.Fatal(err)
				}
				got, err := Load(root, s.Meta.RunID)
				if err == nil || !reflect.DeepEqual(got, Snapshot{}) {
					t.Fatalf("повреждение принято: %+v, %v", got, err)
				}
				if name == "task.md" {
					message := err.Error()
					if !strings.Contains(message, name) || strings.Contains(message, "meta.json") {
						t.Errorf("диагностика должна указывать только на повреждённый %s: %v", name, err)
					}
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
