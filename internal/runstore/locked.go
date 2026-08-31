//go:build darwin || linux

package runstore

import (
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// ErrRunLocked означает, что run уже открыт другим координатором. Ожидания,
// снятия чужой блокировки и удаления lock-файла при этом не происходит.
var ErrRunLocked = errors.New("запуск уже управляется другим координатором")

// ErrRevisionConflict означает, что app-наблюдатель пытался сохранить результат
// для уже изменившегося снимка. Старое событие нужно отбросить и перечитать task.
var ErrRevisionConflict = errors.New("устаревшая ревизия app-задачи")

// LockedRun удерживает flock до Close или выхода процесса. Методы сериализованы;
// значение нельзя копировать. Блокировка рассчитана на локальную ФС macOS/Linux
// и сотрудничающие процессы Lawa, не заменяет права доступа агентов Codex.
// Координатор держит её всё время наблюдения, включая сетевые запросы. После
// Close запрещены любые новые запросы от этого владельца, даже по старому снимку.
type LockedRun struct {
	mu     sync.Mutex
	dir    *os.Root
	lock   *os.File
	runID  string
	failed error // После ошибки сохранения нельзя продолжать по неопределённому снимку.
}

// OpenLocked открывает существующий run без ожидания занятой блокировки, затем
// проверяет все сохранённые входы. Создаёт только coordinator.lock с 0600; этот
// файл никогда не удаляется: иначе два процесса могли бы захватить разные inode.
// Остаток файла после сбоя не мешает открытию: flock освобождается ядром. Каталог
// run и lock-файл нельзя вручную удалять/заменять, пока координатор работает.
func OpenLocked(root, runID string) (*LockedRun, error) {
	dir, err := openRun(root, runID)
	if err != nil {
		return nil, err
	}
	r := &LockedRun{dir: dir, runID: runID}
	fail := func(err error) (*LockedRun, error) { return nil, errors.Join(err, r.Close()) }
	// Root следует внутренним симлинкам даже с O_NOFOLLOW: проверяем имя до
	// открытия, чтобы O_CREATE не создал файл по подставной ссылке внутри run.
	info, err := dir.Lstat("coordinator.lock")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if err == nil && !info.Mode().IsRegular() {
		return fail(fmt.Errorf("coordinator.lock должен быть обычным файлом"))
	}
	r.lock, err = dir.OpenFile("coordinator.lock", os.O_CREATE|os.O_RDWR|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return fail(err)
	}
	info, err = r.lock.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("coordinator.lock должен быть обычным файлом"))
	}
	if err = syscall.Flock(int(r.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = ErrRunLocked
		}
		return fail(fmt.Errorf("блокировка запуска %q: %w", runID, err))
	}
	if _, err = load(dir, runID); err != nil {
		return fail(err)
	}
	return r, nil
}

// Close освобождает файловые дескрипторы и блокировку. Повтор безопасен; память
// и служебные файлы не удаляются. Сначала координатор прекращает сетевую работу.
func (r *LockedRun) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.lock != nil {
		err = r.lock.Close()
		r.lock = nil
	}
	if r.dir != nil {
		err = errors.Join(err, r.dir.Close())
		r.dir = nil
	}
	return err
}

// check запрещает использование закрытого владельца и продолжение после сбоя
// записи. Вызывается только под mu; первичная причина остаётся в errors.Is/As.
func (r *LockedRun) check() error {
	if r.dir == nil {
		return os.ErrClosed
	}
	return r.failed
}

// Load возвращает независимый снимок под блокировкой. Правки результата не
// меняют хранилище; перед планированием всё равно нужна сверка статусов с Codex.
func (r *LockedRun) Load() (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return Snapshot{}, err
	}
	return load(r.dir, r.runID)
}

// Update меняет только состояние и связь одного шага. Pending может перейти
// только в Starting без ID: успешная запись этого намерения предшествует запросу
// создания чата. Из Starting можно привязать подтверждённый/восстановленный ID;
// известный ID нельзя заменить или стереть. Обратный переход в Pending доступен
// только через ReleaseUnattempted, когда клиент в том же процессе достоверно
// сообщил, что thread/start ещё не пытался отправить.
// Для существующего чата допустим ручной повтор, в том числе Succeeded → Running.
// Повтор той же записи безопасен, но не даёт права повторять сетевой запрос.
// Готовность зависимостей и достоверность статуса проверяет координатор.
//
// Любая ошибка сохранения запрещает новые запросы и дальнейшие Update/Load у
// этого владельца. Нужно Close, повторное открытие и сверка с Codex; ни сброса
// Starting, ни удаления run нет. После ошибки Sync каталога новая meta уже может
// быть видна: откат к старой версии способен потерять связь с работающим чатом.
func (r *LockedRun) Update(stepID string, state scheduler.State, codexThreadID string) error {
	return r.update(stepID, state, codexThreadID, (*os.File).Sync)
}

// UpdateIfRevision применяет app-native статус только к снимку, который наблюдал
// вызывающий координатор. Успех финализирует кубик: поздний turn остаётся обычным
// чатом Codex App, но не откатывает уже запущенные зависимости workflow.
func (r *LockedRun) UpdateIfRevision(stepID string, state scheduler.State, codexThreadID string, revision uint64) error {
	return r.updateRevision(stepID, state, codexThreadID, &revision, (*os.File).Sync)
}

// Reserve атомарно переводит целую волну готовых Pending-шагов в Starting одним
// новым meta.json. Координатор использует пакетную запись, чтобы отказ диска не
// оставил только часть параллельной волны зарезервированной до сетевых запросов.
// Пустая волна безопасна и ничего не записывает. ID обязаны быть уникальными:
// повтор обычно означает ошибку плана, которую нельзя скрывать идемпотентностью.
func (r *LockedRun) Reserve(stepIDs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	if len(stepIDs) == 0 {
		return nil
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return err
	}
	indices := make(map[string]int, len(s.Meta.Steps))
	for index, step := range s.Meta.Steps {
		indices[step.ID] = index
	}
	seen := make(map[string]bool, len(stepIDs))
	for _, stepID := range stepIDs {
		index, exists := indices[stepID]
		if !exists {
			return fmt.Errorf("нет шага %q", stepID)
		}
		if seen[stepID] {
			return fmt.Errorf("шаг %q повторён в резервировании", stepID)
		}
		seen[stepID] = true
		if s.Meta.Steps[index].State != scheduler.Pending || s.Meta.Steps[index].CodexThreadID != "" {
			return fmt.Errorf("шаг %q уже запускался и не может быть зарезервирован", stepID)
		}
		s.Meta.Steps[index].State = scheduler.Starting
	}
	if err = s.validate(r.runID); err != nil {
		return err
	}
	if err = saveMetadata(r.dir, s.Meta, (*os.File).Sync); err != nil {
		r.failed = fmt.Errorf("сохранение запуска %q: %w; остановите новые запросы и восстановите состояние после повторного открытия", r.runID, err)
		return r.failed
	}
	return nil
}

// ReleaseUnattempted снимает резервирование ровно одного шага после безопасного
// отказа до thread/start. Метод принимает только Starting без CodexThreadID и
// возвращает его в Pending, чтобы следующий явный resume мог повторить создание.
//
// Вызывающий код обязан иметь живое подтверждение CreationAttempted=false именно
// от операции, ради которой был вызван Reserve. Одного сохранённого Starting без
// ID после перезапуска недостаточно: процесс мог отправить thread/start и потерять
// ответ, поэтому такой снимок остаётся неоднозначным и не сбрасывается автоматически.
func (r *LockedRun) ReleaseUnattempted(stepID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return err
	}
	index := -1
	for i, step := range s.Meta.Steps {
		if step.ID == stepID {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("нет шага %q", stepID)
	}
	step := s.Meta.Steps[index]
	if step.State != scheduler.Starting || step.CodexThreadID != "" {
		return fmt.Errorf("шаг %q: снять можно только неподтверждённое резервирование Starting без ID чата", stepID)
	}
	s.Meta.Steps[index].State = scheduler.Pending
	if err = s.validate(r.runID); err != nil {
		return err
	}
	if err = saveMetadata(r.dir, s.Meta, (*os.File).Sync); err != nil {
		r.failed = fmt.Errorf("сохранение запуска %q: %w; остановите новые запросы и восстановите состояние после повторного открытия", r.runID, err)
		return r.failed
	}
	return nil
}

// WriteMemory атомарно заменяет память одного известного кубика. App-native
// координатор вызывает метод до сохранения Succeeded: зависимый кубик не должен
// стартовать, пока финальный ответ предшественника ещё не сохранён. Повтор с тем
// же текстом безопасен после потери ответа CLI. Произвольный путь не принимается,
// имя выводится только из уже проверенного внутреннего ThreadID.
func (r *LockedRun) WriteMemory(stepID string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("память шага %q должна быть текстом UTF-8", stepID)
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return err
	}
	threadID := ""
	for _, step := range s.Meta.Steps {
		if step.ID == stepID {
			threadID = step.ThreadID
			break
		}
	}
	if threadID == "" {
		return fmt.Errorf("нет шага %q", stepID)
	}
	if err = saveRunFile(r.dir, filepath.Join("memory", threadID+".md"), data, (*os.File).Sync); err != nil {
		r.failed = fmt.Errorf("сохранение памяти запуска %q: %w; остановите новые запросы и восстановите состояние после повторного открытия", r.runID, err)
		return r.failed
	}
	return nil
}

// ClaimAppCreation атомарно выдаёт право ровно на одну внешнюю попытку создания
// app-задачи. Маркер намеренно остаётся после bind: при потере ответа или запуске
// второго координатора повторный create запрещён, а связь восстанавливается по title.
func (r *LockedRun) ClaimAppCreation(stepID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return false, err
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return false, err
	}
	for _, step := range s.Meta.Steps {
		if step.ID != stepID {
			continue
		}
		if step.State != scheduler.Starting || step.CodexThreadID != "" {
			return false, fmt.Errorf("шаг %q не ожидает создания app-задачи", stepID)
		}
		f, createErr := r.dir.OpenFile("app-create-"+step.ThreadID, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(createErr, os.ErrExist) {
			return false, nil
		}
		if createErr != nil {
			return false, createErr
		}
		if err = errors.Join(f.Sync(), f.Close()); err != nil {
			r.failed = fmt.Errorf("сохранение claim запуска %q: %w", r.runID, err)
			return false, r.failed
		}
		if err = r.syncDirectory(); err != nil {
			r.failed = fmt.Errorf("синхронизация claim запуска %q: %w", r.runID, err)
			return false, r.failed
		}
		return true, nil
	}
	return false, fmt.Errorf("нет шага %q", stepID)
}

// ClaimAppContinuation выдаёт право ровно на одно сообщение continue для
// конкретного interrupted turn. Маркер публикуется до внешнего app-вызова:
// потеря ответа send_message_to_thread поэтому оставляет только необходимость
// перечитать тот же чат, а не риск отправить второе продолжение. Новый turn имеет
// другой digest и может быть отдельно восстановлен после нового наблюдения.
func (r *LockedRun) ClaimAppContinuation(stepID, turnID string) (bool, error) {
	if !validText(turnID) {
		return false, errors.New("app-продолжение требует непустой turn-id UTF-8")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return false, err
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return false, err
	}
	for _, step := range s.Meta.Steps {
		if step.ID != stepID {
			continue
		}
		if step.State != scheduler.Cancelled || step.CodexThreadID == "" {
			return false, fmt.Errorf("шаг %q не содержит привязанный interrupted turn", stepID)
		}
		digest := sha256.Sum256([]byte(turnID))
		name := fmt.Sprintf("app-continue-%s-%x", step.ThreadID, digest[:])
		f, createErr := r.dir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(createErr, os.ErrExist) {
			return false, nil
		}
		if createErr != nil {
			return false, createErr
		}
		if err = errors.Join(f.Sync(), f.Close()); err != nil {
			r.failed = fmt.Errorf("сохранение claim продолжения запуска %q: %w", r.runID, err)
			return false, r.failed
		}
		if err = r.syncDirectory(); err != nil {
			r.failed = fmt.Errorf("синхронизация claim продолжения запуска %q: %w", r.runID, err)
			return false, r.failed
		}
		return true, nil
	}
	return false, fmt.Errorf("нет шага %q", stepID)
}

// ResetAppCreationClaim удаляет защитный маркер только по явному решению
// пользователя повторить неопределённую попытку create_thread. Автоматически
// вызывать этот метод нельзя: задача могла быть создана, а её ответ — потерян,
// поэтому новый create способен породить дубль. Перед вызовом управляющий чат
// обязан повторно искать детерминированный title и объяснить пользователю риск.
//
// Сброс допустим только для Starting без сохранённого CodexThreadID. После bind
// identity считается установленной навсегда, даже если позднее задача завершилась.
func (r *LockedRun) ResetAppCreationClaim(stepID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return err
	}
	for _, step := range s.Meta.Steps {
		if step.ID != stepID {
			continue
		}
		if step.State != scheduler.Starting || step.CodexThreadID != "" {
			return fmt.Errorf("шаг %q: claim можно сбросить только до app-bind", stepID)
		}
		name := "app-create-" + step.ThreadID
		info, statErr := r.dir.Lstat(name)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return fmt.Errorf("шаг %q: claim ещё не выдавался", stepID)
			}
			return statErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("шаг %q: маркер claim должен быть обычным файлом", stepID)
		}
		if err = r.dir.Remove(name); err != nil {
			return fmt.Errorf("удалить claim шага %q: %w", stepID, err)
		}
		if err = r.syncDirectory(); err != nil {
			r.failed = fmt.Errorf("синхронизация сброса claim запуска %q: %w", r.runID, err)
			return r.failed
		}
		return nil
	}
	return fmt.Errorf("нет шага %q", stepID)
}

// syncDirectory сохраняет создание или удаление служебного имени в каталоге run.
// Открытый дескриптор всегда закрывается, в том числе после ошибки Sync.
func (r *LockedRun) syncDirectory() error {
	dir, err := r.dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// update принимает Sync явно для проверки отказов до и после публикации meta
// без глобальных подмен или повреждения диска; обычные вызовы используют File.Sync.
func (r *LockedRun) update(stepID string, state scheduler.State, chat string, syncFile func(*os.File) error) error {
	return r.updateRevision(stepID, state, chat, nil, syncFile)
}

func (r *LockedRun) updateRevision(stepID string, state scheduler.State, chat string, expected *uint64, syncFile func(*os.File) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	s, err := load(r.dir, r.runID)
	if err != nil {
		return err
	}
	index := -1
	for i, step := range s.Meta.Steps {
		if step.ID == stepID {
			index = i
		}
	}
	if index < 0 {
		return fmt.Errorf("нет шага %q", stepID)
	}
	old := s.Meta.Steps[index]
	if expected != nil && old.Revision != *expected {
		return fmt.Errorf("шаг %q: %w: ожидалась %d, сохранена %d", stepID, ErrRevisionConflict, *expected, old.Revision)
	}
	if expected != nil && old.State == scheduler.Succeeded {
		return fmt.Errorf("шаг %q уже финализирован для app-native workflow", stepID)
	}
	if old.State == scheduler.Pending && (state != scheduler.Pending && state != scheduler.Starting || chat != "") {
		return fmt.Errorf("шаг %q: сначала сохраните Starting без ID чата", stepID)
	}
	if old.State != scheduler.Pending && state == scheduler.Pending ||
		old.State != scheduler.Pending && old.State != scheduler.Starting && state == scheduler.Starting ||
		old.CodexThreadID != "" && old.CodexThreadID != chat {
		return fmt.Errorf("шаг %q: нельзя сбросить запуск или изменить известный ID чата", stepID)
	}
	s.Meta.Steps[index].State, s.Meta.Steps[index].CodexThreadID = state, chat
	if expected != nil {
		s.Meta.Steps[index].Revision++
	}
	if err = s.validate(r.runID); err != nil {
		return err
	}
	if err = saveMetadata(r.dir, s.Meta, syncFile); err != nil {
		r.failed = fmt.Errorf("сохранение запуска %q: %w; остановите новые запросы и восстановите состояние после повторного открытия", r.runID, err)
		return r.failed
	}
	return nil
}

// saveMetadata публикует целый JSON через Rename в том же каталоге после
// Write/Sync/Close временного файла с 0600, затем синхронизирует запись каталога.
// До Rename прежняя meta остаётся целой; после него отката нет. Временные файлы
// после аварийного выхода не читаются и не мешают следующей записи с новым ID.
func saveMetadata(dir *os.Root, meta Metadata, syncFile func(*os.File) error) (err error) {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return saveRunFile(dir, "meta.json", data, syncFile)
}

// saveRunFile публикует обычный файл через временный соседний inode. Временное
// имя размещается в том же каталоге, поэтому Rename остаётся атомарным и для
// meta.json, и для памяти. После переименования сохраняется запись именно
// родительского каталога целевого файла.
func saveRunFile(dir *os.Root, target string, data []byte, syncFile func(*os.File) error) (err error) {
	parent := filepath.Dir(target)
	name := filepath.Join(parent, ".lawa-"+newID()+".tmp")
	f, err := dir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := dir.Remove(name); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			err = errors.Join(err, cleanupErr)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = syncFile(f)
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err = dir.Rename(name, target); err != nil {
		return err
	}
	f, err = dir.Open(parent)
	if err != nil {
		return err
	}
	return errors.Join(syncFile(f), f.Close())
}
