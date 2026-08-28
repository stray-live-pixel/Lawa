// Package runstore создаёт и читает папки запусков без обращения к Codex.
// Обновление статусов и блокировка координатора здесь пока не реализованы.
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

	"github.com/stray-live-pixel/flows-2/internal/scheduler"
	"github.com/stray-live-pixel/flows-2/internal/workflow"
)

// ErrInitiatorAsExecutor означает нарушение внутреннего инварианта: чат постановки
// задачи не может быть чатом исполнителя. Ошибка распознаётся через errors.Is;
// конкретные runId и ID шага добавляются в месте проверки для диагностики.
//
// На момент добавления защиты (2026-08-28) это теоретический крайний случай,
// воспроизводимый только искусственной подстановкой ID в TestMetadataStates.
// Реальный сбой не наблюдался: Create оставляет CodexThreadID пустым, а создание
// чатов и запись их ID ещё не реализованы. При штатной работе корректного кода
// записи эта ситуация невозможна. Возможные причины вне этого условия — баг
// будущего адаптера/сохранения (перепутаны InitiatorThreadID и CodexThreadID) либо
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
// CWD должен указывать на существующую папку. Проверка подключения — вне пакета.
type Input struct {
	WorkflowJSON      []byte
	Task, Comment     string
	CWD               string
	InitiatorThreadID string
}

// Metadata — версия формата и постоянные связи запуска. State использует
// внутренний словарь планировщика, а не статусы протокола Codex.
type Metadata struct {
	Version           int    `json:"version"`
	RunID             string `json:"runId"`
	CWD               string `json:"cwd"`
	InitiatorThreadID string `json:"initiatorThreadId"`
	Steps             []Step `json:"steps"`
}

// Step связывает ID из графа с отдельным файлом памяти и будущим чатом Codex.
// Произвольный ID из workflow никогда не используется как имя файла.
type Step struct {
	ID            string          `json:"id"`
	ThreadID      string          `json:"threadId"`
	CodexThreadID string          `json:"codexThreadId"`
	State         scheduler.State `json:"state"`
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
	s := Snapshot{Workflow: w, Task: fmt.Sprintf("# Постановка задачи\n\n%s\n\n# Комментарий пользователя\n\n%s\n", in.Task, in.Comment)}
	s.Meta = Metadata{Version: 1, RunID: newID(), CWD: cwd, InitiatorThreadID: in.InitiatorThreadID}
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
	// Абсолютный путь позволяет находить родителя и для root=".". Пустой путь
	// по-прежнему недопустим: Abs иначе незаметно превратил бы его в текущую папку.
	if root == "" {
		return Snapshot{}, fmt.Errorf("нужна папка хранения root")
	}
	if root, err = filepath.Abs(root); err != nil {
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

// Load читает только сохранённые файлы: нет исходного workflow, автосоздания,
// очистки памяти или подстановки Pending вместо потерянного статуса. os.Root
// запрещает выход через симлинки; данные должны быть обычными файлами.
// Отсутствующий cwd проверяет будущий исполнитель, а не чтение истории запуска.
func Load(root, runID string) (Snapshot, error) {
	if !validID(runID) {
		return Snapshot{}, fmt.Errorf("некорректный runId %q", runID)
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		return Snapshot{}, err
	}
	defer base.Close()
	dir, err := base.OpenRoot(runID)
	if err != nil {
		return Snapshot{}, err
	}
	defer dir.Close()
	var s Snapshot
	meta, err := readFile(dir, "meta.json")
	if err != nil {
		return Snapshot{}, err
	}
	if err = json.Unmarshal(meta, &s.Meta, json.RejectUnknownMembers(true)); err != nil {
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
	if m.Version != 1 || m.RunID != runID || !filepath.IsAbs(m.CWD) || !validText(m.CWD) || strings.ContainsRune(m.CWD, 0) ||
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
