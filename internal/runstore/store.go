// Package runstore хранит папки запусков без обращения к Codex. На macOS/Linux
// OpenLocked даёт координатору исключительное право обновлять состояния шагов.
package runstore

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// StatusImageFilename — принадлежащее layout run стабильное имя последней PNG-
// схемы. Runstore использует его для безопасного чтения без произвольного пути.
const StatusImageFilename = "workflow-status.png"

// ErrInitiatorAsExecutor означает нарушение внутреннего инварианта: чат постановки
// задачи не может быть чатом исполнителя. Ошибка распознаётся через errors.Is;
// конкретные runId и ID шага добавляются в месте проверки для диагностики.
//
// На момент добавления защиты (2026-08-28) это теоретический крайний случай,
// воспроизводимый только искусственной подстановкой ID в TestMetadataStates.
// Реальный сбой не наблюдался: Create оставляет CodexThreadID пустым, а координатор
// передаёт в Update только ответ thread/start. При штатной работе кода
// записи эта ситуация невозможна. Возможные причины вне этого условия — баг
// координатора/сохранения (перепутаны InitiatorThreadID и CodexThreadID) либо
// ручное изменение meta.json. Это не обычная ошибка пользовательского ввода.
//
// Обработчик должен остановить продолжение этого run и сохранить данные для
// диагностики записи связи. Load возвращает пустой Snapshot и ничего не исправляет.
// Нельзя сбрасывать шаг в Pending, угадывать ID или автоматически создавать нового
// агента: исходный исполнитель может уже работать, а ошибочна только запись о нём.
// Без независимой достоверной связи узнать правильный чат невозможно; пользователь
// также не обязан знать его ID. При разборе нужно проверить код сохранения связи
// и исходный ответ создания чата, если он доступен, а не маскировать ошибку повтором.
var ErrInitiatorAsExecutor = errors.New("в meta.json чат постановки задачи указан вместо чата исполнителя; " +
	"запуск нельзя продолжать или автоматически перезапускать")

// Input — общий вход нового запуска. WorkflowJSON сохраняется побайтно; Task
// и Comment записываются отдельными разделами task.md без обрезки пробелов.
// ParentRunID необязателен, но обязан вести в существующий run того же root.
// CWD должен указывать на существующую папку. Проверка подключения — вне пакета.
type Input struct {
	WorkflowJSON      []byte
	Task, Comment     string
	CWD               string
	InitiatorThreadID string
	ParentRunID       string
}

// Metadata — версия формата и постоянные связи запуска. ParentRunID появился в
// v2; пустое значение у прежнего v1 означает корень дерева. State использует
// внутренний словарь планировщика, а не статусы протокола Codex.
type Metadata struct {
	Version           int    `json:"version"`
	RunID             string `json:"runId"`
	ParentRunID       string `json:"parentRunId,omitempty"`
	CWD               string `json:"cwd"`
	InitiatorThreadID string `json:"initiatorThreadId"`
	Steps             []Step `json:"steps"`
}

// Step связывает ID из графа с отдельным файлом памяти и чатом Codex.
// Произвольный ID из workflow никогда не используется как имя файла. Revision
// монотонно растёт после каждого принятого app-update и позволяет отклонять
// запоздавшие наблюдения параллельных управляющих чатов. Старый coordinator не
// передаёт ожидаемую ревизию, поэтому для его запусков поле может оставаться нулём.
type Step struct {
	ID            string          `json:"id"`
	ThreadID      string          `json:"threadId"`
	CodexThreadID string          `json:"codexThreadId"`
	State         scheduler.State `json:"state"`
	Revision      uint64          `json:"revision,omitempty"`
}

// Snapshot содержит сохранённый вход и последний известный снимок состояний.
// Это не подтверждение текущего статуса чатов: перед исполнением нужна сверка
// с Codex. Чтение снимка не резервирует запуск и не заменяет блокировку run.
type Snapshot struct {
	Workflow workflow.Workflow
	Task     string
	Meta     Metadata
}

// Create проверяет входы до записи и создаёт новый run под root (обычно
// ~/.light-ai-workflows). Root выбирает вызывающий код; пакет не меняет cwd.
// Каталоги создаются с 0700, файлы с 0600; права существующего root не меняются.
// Root должен быть доверенным хранилищем: chmod не изолирует агентов того же UID.
// Вызывающий код не должен параллельно изменять Input.WorkflowJSON.
// До создания run сохраняем имена каталогов root в их родителях. Отказ Sync
// оставляет только общие папки без нового run; повтор снова сохраняет их имена.
//
// Mkdir резервирует новый ID без перезаписи. Meta публикуется переименованием
// только после записи и Sync остальных файлов. До этого Load возвращает ошибку.
// При обычной ошибке пробуем удалить только созданную здесь папку и возвращаем
// *CreateError с причиной и результатом очистки. Сбой процесса или питания может
// оставить неполную папку: defer не гарантирован, а удаление не синхронизируется.
// Load такую папку не восстанавливает автоматически.
// Запускать агентов разрешено только после успешного возврата Create.
func Create(root string, in Input) (Snapshot, error) {
	return create(root, in, syncDir)
}

// create принимает синхронизацию каталогов явно, чтобы тесты могли воспроизвести
// отказ диска без глобальных подмен, влияющих на параллельные вызовы Create.
func create(root string, in Input, syncDirectory func(string) error) (_ Snapshot, err error) {
	w, err := workflow.Decode(bytes.NewReader(in.WorkflowJSON))
	if err != nil {
		return Snapshot{}, err
	}
	if !validText(in.Task) || !utf8.ValidString(in.Comment) || !validText(in.InitiatorThreadID) || !validText(in.CWD) {
		return Snapshot{}, fmt.Errorf("нужны постановка, cwd и ID чата-инициатора; текст должен быть UTF-8")
	}
	cwd, err := filepath.Abs(in.CWD)
	if err != nil {
		return Snapshot{}, err
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return Snapshot{}, err
	}
	if !info.IsDir() {
		return Snapshot{}, fmt.Errorf("cwd %q не является папкой", cwd)
	}
	// Root нормализуется до проверки родителя: одна и та же абсолютная папка
	// определяет область связей и последующее место записи нового run.
	if root == "" {
		return Snapshot{}, fmt.Errorf("нужна папка хранения root")
	}
	if root, err = filepath.Abs(root); err != nil {
		return Snapshot{}, err
	}
	if in.ParentRunID != "" {
		if err = validateParentChain(root, in.ParentRunID); err != nil {
			return Snapshot{}, err
		}
	}
	s := Snapshot{Workflow: w, Task: fmt.Sprintf("# Постановка задачи\n\n%s\n\n# Комментарий пользователя\n\n%s\n", in.Task, in.Comment)}
	s.Meta = Metadata{Version: 2, RunID: newID(), ParentRunID: in.ParentRunID, CWD: cwd, InitiatorThreadID: in.InitiatorThreadID}
	for _, step := range w.Steps {
		s.Meta.Steps = append(s.Meta.Steps, Step{ID: step.ID, ThreadID: newID(), State: scheduler.Pending})
	}
	if err = s.validate(s.Meta.RunID); err != nil {
		return Snapshot{}, err
	}
	meta, err := json.Marshal(s.Meta)
	if err != nil {
		return Snapshot{}, err
	}
	if err = mkdirAllSynced(root, syncDirectory); err != nil {
		return Snapshot{}, err
	}
	dir := filepath.Join(root, s.Meta.RunID)
	if err = os.Mkdir(dir, 0o700); err != nil {
		return Snapshot{}, err
	}
	defer func() {
		if err != nil {
			// Только успешный Mkdir выше даёт право удалять именно этот новый run.
			// Даже опубликованный meta.json не означает успех Create: последующий
			// Sync мог отказать. При двойном сбое сохраняем обе причины отдельно.
			err = &CreateError{RunDir: dir, Cause: err, CleanupErr: os.RemoveAll(dir)}
		}
	}()
	if err = os.Mkdir(filepath.Join(dir, "memory"), 0o700); err != nil {
		return Snapshot{}, err
	}
	files := map[string][]byte{"workflow.json": in.WorkflowJSON, "task.md": []byte(s.Task), "meta.json.tmp": meta}
	for _, step := range s.Meta.Steps {
		files[filepath.Join("memory", step.ThreadID+".md")] = nil
	}
	for name, data := range files {
		if err = writeNewFile(filepath.Join(dir, name), data); err != nil {
			return Snapshot{}, err
		}
	}
	if err = syncDirectory(filepath.Join(dir, "memory")); err != nil {
		return Snapshot{}, err
	}
	if err = os.Rename(filepath.Join(dir, "meta.json.tmp"), filepath.Join(dir, "meta.json")); err != nil {
		return Snapshot{}, err
	}
	// Сохраняем записи обоих каталогов: имя meta внутри run и имя самого run.
	if err = errors.Join(syncDirectory(dir), syncDirectory(root)); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// validateParentChain проверяет всю уже сохранённую цепочку до создания ребёнка.
// Новый run ещё не имеет ID и потому не может стать собственным предком штатным
// API, однако ручное повреждение двух старых meta.json не должно расширять цикл.
func validateParentChain(root, parentRunID string) error {
	seen := make(map[string]bool)
	for current := parentRunID; current != ""; {
		if seen[current] {
			return fmt.Errorf("родительские run образуют цикл на %q", current)
		}
		seen[current] = true
		parent, err := Load(root, current)
		if err != nil {
			return fmt.Errorf("прочитать родительский run %q: %w", current, err)
		}
		current = parent.Meta.ParentRunID
	}
	return nil
}

// Load читает только сохранённые файлы: нет исходного workflow, автосоздания,
// очистки памяти или подстановки Pending вместо потерянного статуса. os.Root
// запрещает выход через симлинки; данные должны быть обычными файлами.
// Отсутствующий cwd проверяет CLI/Codex preflight, а не чтение истории запуска.
func Load(root, runID string) (Snapshot, error) {
	dir, err := openRun(root, runID)
	if err != nil {
		return Snapshot{}, err
	}
	defer dir.Close()
	return load(dir, runID)
}

// LoadForDashboard читает валидный snapshot для read-only интерфейса, но
// пропускает неизвестные поля JSON. Это позволяет уже запущенному dashboard
// пережить добавление необязательного поля более новой Lawa: старый интерфейс
// покажет известную ему часть данных до собственного перезапуска.
//
// Послабление относится только к лишним членам JSON. Версия формата, известные
// поля, связи workflow, состояния и безопасные имена файлов проходят ту же
// проверку, что и в Load. Координатору этот режим не подходит: при исполнении
// неизвестное поле может содержать семантику, которую старый процесс обязан не
// игнорировать, поэтому все изменяющие run операции используют строгий load.
func LoadForDashboard(root, runID string) (Snapshot, error) {
	dir, err := openRun(root, runID)
	if err != nil {
		return Snapshot{}, err
	}
	defer dir.Close()
	return loadForDashboard(dir, runID)
}

// ReadMemory возвращает память только существующего кубика из валидного run.
// os.Root удерживает чтение внутри каталога даже при конкурентной подмене пути;
// проверка обычного файла не позволяет использовать симлинк на чужие данные.
func ReadMemory(root, runID, threadID string) ([]byte, error) {
	dir, err := openRun(root, runID)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	snapshot, err := loadForDashboard(dir, runID)
	if err != nil {
		return nil, err
	}
	found := false
	for _, step := range snapshot.Meta.Steps {
		found = found || step.ThreadID == threadID
	}
	if !found {
		return nil, fmt.Errorf("неизвестный threadId памяти %q", threadID)
	}
	return readFile(dir, filepath.Join("memory", threadID+".md"))
}

// ReadStatusImage читает только стабильный PNG статуса. Отдельная функция вместо
// произвольного относительного пути сохраняет узкую read-only границу dashboard.
func ReadStatusImage(root, runID string) ([]byte, error) {
	dir, err := openRun(root, runID)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if _, err = loadForDashboard(dir, runID); err != nil {
		return nil, err
	}
	return readFile(dir, StatusImageFilename)
}

// RemoveUnstarted удаляет только полностью созданный run, которому координатор
// ещё не успел выдать ни одного задания. Откат допустим лишь между Create и
// публикацией runId в серии; для уже известного пользователю run эту функцию
// вызывать нельзя, даже если его шаги пока выглядят как Pending.
func RemoveUnstarted(root, runID string) error {
	snapshot, err := Load(root, runID)
	if err != nil {
		return fmt.Errorf("проверить откат run %q: %w", runID, err)
	}
	for _, step := range snapshot.Meta.Steps {
		if step.State != scheduler.Pending || step.CodexThreadID != "" {
			return fmt.Errorf("откат run %q запрещён: шаг %q уже передан исполнителю", runID, step.ID)
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, runID)
	if err = os.RemoveAll(dir); err != nil {
		return fmt.Errorf("удалить незапущенный run %q из %q: %w", runID, dir, err)
	}
	return syncDir(root)
}

// openRun ограничивает все операции каталогом выбранного run, в том числе после
// его открытия координатором. Доверенный root нельзя подменять во время работы.
func openRun(root, runID string) (*os.Root, error) {
	if !validID(runID) {
		return nil, fmt.Errorf("некорректный runId %q", runID)
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	return base.OpenRoot(runID)
}

// load читает самостоятельный снимок из уже открытого каталога и отклоняет
// неизвестные поля. Координатор вызывает её под блокировкой; обычный Load может
// увидеть старую или новую атомарно опубликованную meta.
func load(dir *os.Root, runID string) (Snapshot, error) {
	return loadSnapshot(dir, runID, true)
}

// loadForDashboard отличается от load только совместимостью с добавочными
// полями JSON. Вся содержательная validate ниже остаётся общей и строгой.
func loadForDashboard(dir *os.Root, runID string) (Snapshot, error) {
	return loadSnapshot(dir, runID, false)
}

// loadSnapshot сосредотачивает общий путь чтения, чтобы dashboard не получил
// упрощённую копию проверок безопасности при поддержке новых полей.
func loadSnapshot(dir *os.Root, runID string, rejectUnknownMembers bool) (Snapshot, error) {
	var s Snapshot
	meta, err := readFile(dir, "meta.json")
	if err != nil {
		return Snapshot{}, err
	}
	if rejectUnknownMembers {
		err = json.Unmarshal(meta, &s.Meta, json.RejectUnknownMembers(true))
	} else {
		err = json.Unmarshal(meta, &s.Meta)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("meta.json: %w", err)
	}
	data, err := readFile(dir, "workflow.json")
	if err != nil {
		return Snapshot{}, err
	}
	if s.Workflow, err = workflow.Decode(bytes.NewReader(data)); err != nil {
		return Snapshot{}, err
	}
	data, err = readFile(dir, "task.md")
	if err != nil {
		return Snapshot{}, err
	}
	s.Task = string(data)
	if err = s.validate(runID); err != nil {
		return Snapshot{}, err
	}
	for _, step := range s.Meta.Steps {
		info, err := dir.Lstat(filepath.Join("memory", step.ThreadID+".md"))
		if err != nil {
			return Snapshot{}, err
		}
		if !info.Mode().IsRegular() {
			return Snapshot{}, fmt.Errorf("память шага %q должна быть обычным файлом", step.ID)
		}
	}
	return s, nil
}

// validate отклоняет повреждённую постановку, несовместимую версию, потерянные
// или дублированные связи и состояния, способные создать повторный чат.
// Starting без ID оставляем как неопределённый результат создания,
// а не превращаем в новый Pending.
func (s Snapshot) validate(runID string) error {
	m := s.Meta
	if (m.Version != 1 && m.Version != 2) || m.Version == 1 && m.ParentRunID != "" ||
		m.RunID != runID || m.ParentRunID == m.RunID || m.ParentRunID != "" && !validID(m.ParentRunID) ||
		!filepath.IsAbs(m.CWD) || !validText(m.CWD) || strings.ContainsRune(m.CWD, 0) ||
		!validText(m.InitiatorThreadID) || len(m.Steps) != len(s.Workflow.Steps) {
		return fmt.Errorf("повреждены входы, версия или состав meta.json")
	}
	// Постановка хранится отдельно: её повреждение не должно направлять
	// диагностику к исправному meta.json. Само правило проверки не меняется.
	if !validText(s.Task) {
		return fmt.Errorf("task.md: постановка должна быть непустым текстом UTF-8")
	}
	states := make(map[string]scheduler.State)
	threads, chats := make(map[string]bool), make(map[string]bool)
	for _, step := range m.Steps {
		if !validID(step.ThreadID) || threads[step.ThreadID] {
			return fmt.Errorf("шаг %q: неверный или повторный threadId", step.ID)
		}
		threads[step.ThreadID] = true
		// Чат постановки задачи управляет запуском, но не исполняет кубики.
		// Проверяем до состояния: конфликт заслуживает отдельной ошибки и в Pending.
		// Контекст и запрет повтора — в ErrInitiatorAsExecutor.
		if step.CodexThreadID == m.InitiatorThreadID {
			return fmt.Errorf("запуск %q, шаг %q: %w", m.RunID, step.ID, ErrInitiatorAsExecutor)
		}
		requiresChat := step.State != scheduler.Pending && step.State != scheduler.Starting
		if step.State == scheduler.Pending && step.CodexThreadID != "" || requiresChat && !validText(step.CodexThreadID) {
			return fmt.Errorf("шаг %q: состояние не соответствует codexThreadId", step.ID)
		}
		if step.CodexThreadID != "" {
			if !validText(step.CodexThreadID) || chats[step.CodexThreadID] {
				return fmt.Errorf("шаг %q: неверный или повторный codexThreadId", step.ID)
			}
			chats[step.CodexThreadID] = true
		}
		states[step.ID] = step.State
	}
	_, err := scheduler.Evaluate(s.Workflow, states)
	return err
}

// newID даёт 128 случайных бит в безопасном для имени файла формате. В Go 1.27
// rand.Read всегда заполняет буфер; сбой источника случайности завершает процесс.
func newID() string {
	var id [16]byte
	rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

// validID не позволяет интерпретировать сохранённый идентификатор как путь.
func validID(id string) bool {
	_, err := hex.DecodeString(id)
	return len(id) == 32 && err == nil && id == strings.ToLower(id)
}

// validText проверяет обязательный текст, не изменяя его содержимое.
func validText(s string) bool { return utf8.ValidString(s) && strings.TrimSpace(s) != "" }

// writeNewFile запрещает перезапись и сохраняет содержимое до публикации meta.
// Ошибки записи, Sync и Close запрещают успешное завершение Create.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// mkdirAllSynced создаёт абсолютный path с 0700 и сохраняет его имя в родителе.
// Спускаемся от существующего предка: перед созданием дочерней папки имя её
// родителя уже сохранено. Иначе после сбоя мог бы исчезнуть весь вложенный root.
// Существующую папку тоже синхронизируем через родителя: предыдущий или параллельный
// Create мог создать её, но ещё не завершить Sync. Поэтому повтор после ошибки
// не пропускает сохранение оставшейся папки. Общие каталоги при отказе не удаляем.
// Предки выше найденной существующей папки считаются ранее сохранёнными; вызывающий
// код не должен удалять или перемещать предков доверенного root во время Create.
func mkdirAllSynced(path string, syncDirectory func(string) error) error {
	parent := filepath.Dir(path)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) && parent != path {
		if err := mkdirAllSynced(parent, syncDirectory); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// MkdirAll также проверяет, что существующий путь — каталог, и допускает
	// создание той же папки другим Create между Stat и этой операцией.
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if parent == path {
		return nil // У корня файловой системы нет отдельного имени в родителе.
	}
	return syncDirectory(parent)
}

// syncDir сохраняет записи каталога после создания файлов или переименования.
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// readFile не принимает каталог или симлинк вместо одного из входов запуска.
func readFile(dir *os.Root, name string) ([]byte, error) {
	info, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s должен быть обычным файлом", name)
	}
	return dir.ReadFile(name)
}
