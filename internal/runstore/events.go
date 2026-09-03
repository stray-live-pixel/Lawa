//go:build darwin || linux

package runstore

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const eventsFilename = "events.jsonl"

// eventReadBatchSize ограничивает память одного прохода `logs --follow`.
// Нормализованное событие значительно меньше мегабайта; отсутствие перевода
// строки во всём batch поэтому означает повреждённую или неподдерживаемую запись.
const eventReadBatchSize = 1024 * 1024

// RuntimeEvent — нормализованное событие Lawa. Message содержит короткую
// диагностику для status и logs, а Content — выбранный подробный поток для
// локальной боковой панели Dashboard. В Content могут попасть команда, её вывод
// или сообщение агента, поэтому файл остаётся приватным 0600, а FormatEvent
// намеренно это поле не печатает. Сырые reasoning, diff и произвольные payload
// App Server по-прежнему не сохраняются. Usage содержит только числовые счётчики
// без исходного JSON.
type RuntimeEvent struct {
	Time     time.Time        `json:"time"`
	RunID    string           `json:"runId"`
	StepID   string           `json:"stepId,omitempty"`
	VisitID  string           `json:"visitId,omitempty"`
	ThreadID string           `json:"threadId,omitempty"`
	TurnID   string           `json:"turnId,omitempty"`
	Kind     string           `json:"kind"`
	State    string           `json:"state,omitempty"`
	ItemID   string           `json:"itemId,omitempty"`
	ItemType string           `json:"itemType,omitempty"`
	Message  string           `json:"message,omitempty"`
	Content  string           `json:"content,omitempty"`
	PID      int              `json:"pid,omitempty"`
	ExitCode *int             `json:"exitCode,omitempty"`
	Signal   string           `json:"signal,omitempty"`
	Usage    map[string]int64 `json:"usage,omitempty"`
}

// EventSummary сворачивает журнал в последние полезные поля одного кубика.
// Это производное read-only представление; meta.json остаётся источником истины
// для планировщика, а events.jsonl — источником диагностики оператора.
type EventSummary struct {
	StepID, VisitID, ThreadID, TurnID, Kind, State, Message string
	LastActivity                                            time.Time
	PID                                                     int
	ExitCode                                                *int
	Signal                                                  string
	ActiveItemTypes                                         []string
}

// AppendEvent дописывает одну строку под той же блокировкой, что и meta.json.
// Один LockedRun является единственным writer, а O_APPEND не оставляет читателю
// промежуточную перезапись истории. Ошибка записи делает владельца failed: Lawa
// не продолжает запуск вслепую без обещанной наблюдаемости.
func (r *LockedRun) AppendEvent(event RuntimeEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	snapshot, err := load(r.dir, r.runID)
	if err != nil {
		return err
	}
	if err = validateRuntimeEventReference(snapshot, event, true); err != nil {
		return err
	}
	event.RunID = r.runID
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	} else {
		event.Time = event.Time.UTC()
	}
	if err = normalizeRuntimeEvent(&event); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	created := false
	if info, statErr := r.dir.Lstat(eventsFilename); statErr == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s должен быть обычным файлом", eventsFilename)
	} else if statErr != nil {
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		created = true
	}
	f, err := r.dir.OpenFile(eventsFilename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if f != nil {
		err = errors.Join(err, f.Close())
	}
	if err == nil && created {
		directory, openErr := r.dir.Open(".")
		if openErr == nil {
			openErr = errors.Join(directory.Sync(), directory.Close())
		}
		err = openErr
	}
	if err != nil {
		r.failed = fmt.Errorf("сохранение журнала запуска %q: %w; новые запросы остановлены, чтобы не потерять наблюдаемость", r.runID, err)
		return r.failed
	}
	return nil
}

// ReadEvents читает только нормализованный журнал выбранного валидного run.
// Отсутствие файла допустимо для запусков старых версий и означает пустую историю.
func ReadEvents(root, runID string) ([]RuntimeEvent, error) {
	return readEvents(root, runID, nil)
}

// readEvents принимает hook между первым подтверждением run и чтением журнала.
// Production его не задаёт; тест воспроизводит публикацию нового visit именно в
// этом окне, не полагаясь на scheduler или задержки файловой системы.
func readEvents(root, runID string, afterInitialLoad func() error) ([]RuntimeEvent, error) {
	dir, err := openRun(root, runID)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if _, err = loadForDashboard(dir, runID); err != nil {
		return nil, err
	}
	if afterInitialLoad != nil {
		if err = afterInitialLoad(); err != nil {
			return nil, err
		}
	}
	info, err := dir.Lstat(eventsFilename)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s должен быть обычным файлом", eventsFilename)
	}
	f, err := dir.Open(eventsFilename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	// Сообщение ограничено при записи, но увеличенный буфер позволяет читать
	// журнал будущей версии с дополнительными числовыми полями без ложной порчи.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var events []RuntimeEvent
	for line := 1; scanner.Scan(); line++ {
		event, parseErr := parseRuntimeEvent(scanner.Bytes(), runID)
		if parseErr != nil {
			err = parseErr
			return nil, fmt.Errorf("%s, строка %d: %w", eventsFilename, line, err)
		}
		events = append(events, event)
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}
	// Visits только добавляются, а их step/thread не меняются. Снимок после
	// прочитанного batch поэтому знает не меньше его событий; снимок до чтения
	// мог ещё не содержать visit, который writer успел опубликовать вместе с
	// первой строкой JSONL между двумя файловыми операциями reader.
	snapshot, err := loadForDashboard(dir, runID)
	if err != nil {
		return nil, err
	}
	for index, event := range events {
		if err = validateRuntimeEventReference(snapshot, event, false); err != nil {
			return nil, fmt.Errorf("%s, строка %d: %w", eventsFilename, index+1, err)
		}
	}
	return events, nil
}

// ReadEventsAfter читает только полные JSONL-записи после byte offset и
// возвращает позицию сразу после последней из них. Неполный хвост не считается
// повреждением: writer мог находиться внутри единственного append, поэтому
// следующий polling повторит чтение с прежней границы строки.
func ReadEventsAfter(root, runID string, offset int64) ([]RuntimeEvent, int64, error) {
	return readEventsAfter(root, runID, offset, nil)
}

// readEventsAfter использует тот же управляемый race-hook, что полное чтение.
// Валидация ссылок выполняется по metadata, перечитанной после готового batch.
func readEventsAfter(root, runID string, offset int64, afterInitialLoad func() error) ([]RuntimeEvent, int64, error) {
	if offset < 0 {
		return nil, offset, errors.New("позиция журнала не может быть отрицательной")
	}
	dir, err := openRun(root, runID)
	if err != nil {
		return nil, offset, err
	}
	defer dir.Close()
	if _, err = loadForDashboard(dir, runID); err != nil {
		return nil, offset, err
	}
	if afterInitialLoad != nil {
		if err = afterInitialLoad(); err != nil {
			return nil, offset, err
		}
	}
	info, err := dir.Lstat(eventsFilename)
	if errors.Is(err, os.ErrNotExist) && offset == 0 {
		return nil, 0, nil
	}
	if err != nil {
		return nil, offset, err
	}
	if !info.Mode().IsRegular() || info.Size() < offset {
		return nil, offset, fmt.Errorf("%s был заменён или усечён", eventsFilename)
	}
	f, err := dir.Open(eventsFilename)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, eventReadBatchSize))
	if readErr != nil {
		return nil, offset, readErr
	}
	lastNewline := bytes.LastIndexByte(data, '\n')
	if lastNewline < 0 {
		if len(data) == eventReadBatchSize {
			return nil, offset, fmt.Errorf("%s содержит запись больше %d байт", eventsFilename, eventReadBatchSize)
		}
		return nil, offset, nil
	}
	complete := data[:lastNewline]
	events := make([]RuntimeEvent, 0, bytes.Count(complete, []byte{'\n'})+1)
	for _, line := range bytes.Split(complete, []byte{'\n'}) {
		event, parseErr := parseRuntimeEvent(line, runID)
		if parseErr != nil {
			return nil, offset, fmt.Errorf("%s после байта %d: %w", eventsFilename, offset, parseErr)
		}
		events = append(events, event)
	}
	snapshot, err := loadForDashboard(dir, runID)
	if err != nil {
		return nil, offset, err
	}
	for _, event := range events {
		if err = validateRuntimeEventReference(snapshot, event, false); err != nil {
			return nil, offset, fmt.Errorf("%s после байта %d: %w", eventsFilename, offset, err)
		}
	}
	return events, offset + int64(lastNewline+1), nil
}

func parseRuntimeEvent(data []byte, runID string) (RuntimeEvent, error) {
	var event RuntimeEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return RuntimeEvent{}, err
	}
	if event.RunID != runID {
		return RuntimeEvent{}, errors.New("неверный runId")
	}
	if err := normalizeRuntimeEvent(&event); err != nil {
		return RuntimeEvent{}, err
	}
	return event, nil
}

// validateRuntimeEventReference связывает новый журнал v4 с одним
// неизменяемым посещением. StepID недостаточно: в цикле один шаг
// имеет несколько thread и turn, которые нельзя смешивать в одну историю.
//
// Пустые thread/turn допустимы: process_started и ранняя ошибка могут
// попасть в журнал до ответа thread/start или turn/start. Если ID уже
// известен событию, он обязан совпасть с durable-связью. Привязка
// к конкретному kind намеренно не нужна: протокол событий расширяется.
//
// appendCurrentTurn включает сверку с последним TurnID metadata только на записи.
// При чтении старые turn того же visit остаются валидной историей,
// потому читатель проверяет их scope, но не сравнивает с последним TurnID.
func validateRuntimeEventReference(snapshot Snapshot, event RuntimeEvent, appendCurrentTurn bool) error {
	if snapshot.Meta.Version != 4 {
		if event.VisitID != "" {
			return fmt.Errorf("событие legacy-run не может содержать visitId")
		}
		if event.StepID == "" {
			return nil
		}
		for _, step := range snapshot.Meta.Steps {
			if step.ID == event.StepID {
				return nil
			}
		}
		return fmt.Errorf("событие ссылается на неизвестный шаг %q", event.StepID)
	}

	if event.VisitID == "" || event.StepID == "" {
		return errors.New("событие v4 требует visitId и stepId")
	}
	index, err := findVisit(snapshot.Meta.Visits, event.VisitID)
	if err != nil {
		return fmt.Errorf("событие ссылается на неизвестное посещение %q", event.VisitID)
	}
	visit := snapshot.Meta.Visits[index]
	if event.StepID != visit.StepID {
		return fmt.Errorf("событие visit %q ссылается на шаг %q вместо %q", event.VisitID, event.StepID, visit.StepID)
	}
	if event.ThreadID != "" && event.ThreadID != visit.CodexThreadID {
		return fmt.Errorf("событие visit %q не принадлежит thread %q", event.VisitID, event.ThreadID)
	}
	if event.TurnID != "" && event.ThreadID == "" {
		return fmt.Errorf("событие visit %q не может указывать turn без thread", event.VisitID)
	}
	if appendCurrentTurn && event.TurnID != "" && event.TurnID != visit.TurnID {
		return fmt.Errorf("событие visit %q не принадлежит turn %q", event.VisitID, event.TurnID)
	}
	return nil
}

// SummarizeEvents возвращает по одному последнему снимку на кубик. Legacy
// журнал ключеван по StepID, а v4 — по VisitID: повторные проходы цикла
// отображаются как разные запуски, даже когда StepID совпадает. PID остаётся
// активным только между process_started и process_exited этого же процесса.
// Текущие действия сопоставляются по непрозрачному itemId, но наружу сводка
// отдаёт только уникальные типы: команда, аргументы и результат не нужны для
// понимания прогресса и не должны попадать в status или dashboard.
func SummarizeEvents(events []RuntimeEvent) map[string]EventSummary {
	summaries := make(map[string]EventSummary)
	activeItems := make(map[string]map[string]string)
	for _, event := range events {
		key := event.StepID
		if event.VisitID != "" {
			key = event.VisitID
		}
		if key == "" {
			continue
		}
		summary := summaries[key]
		if event.StepID != "" {
			summary.StepID = event.StepID
		}
		if event.VisitID != "" {
			summary.VisitID = event.VisitID
		}
		if event.ThreadID != "" {
			summary.ThreadID = event.ThreadID
		}
		if event.TurnID != "" {
			summary.TurnID = event.TurnID
		}
		summary.Kind, summary.LastActivity = event.Kind, event.Time
		if event.State != "" {
			summary.State = event.State
		}
		if event.Message != "" {
			summary.Message = event.Message
		}
		switch event.Kind {
		case "process_started":
			summary.PID, summary.ExitCode, summary.Signal = event.PID, nil, ""
			// Новый дочерний процесс не может продолжать незавершённые item
			// предыдущего процесса. Очистка защищает UI от вечного действия,
			// если прежний App Server завершился без process_exited в журнале.
			delete(activeItems, key)
		case "process_exited":
			summary.PID, summary.ExitCode, summary.Signal = 0, event.ExitCode, event.Signal
			delete(activeItems, key)
		case "turn_started":
			// Item принадлежит одному turn. На границе turn старые элементы
			// больше не считаются активными даже после неполного журнала.
			delete(activeItems, key)
		case "turn_completed":
			delete(activeItems, key)
		case "item_started":
			if event.ItemID != "" && event.ItemType != "" {
				items := activeItems[key]
				if items == nil {
					items = make(map[string]string)
					activeItems[key] = items
				}
				items[event.ItemID] = event.ItemType
			}
		case "item_completed":
			if items := activeItems[key]; items != nil && event.ItemID != "" {
				delete(items, event.ItemID)
				if len(items) == 0 {
					delete(activeItems, key)
				}
			}
		}
		summaries[key] = summary
	}
	for key, items := range activeItems {
		// Несколько параллельных item одного типа должны выглядеть как одно
		// понятное действие. При этом map по ID выше не снимает тип, пока не
		// завершится последний item этого типа.
		uniqueTypes := make(map[string]struct{}, len(items))
		for _, itemType := range items {
			uniqueTypes[itemType] = struct{}{}
		}
		types := make([]string, 0, len(uniqueTypes))
		for itemType := range uniqueTypes {
			types = append(types, itemType)
		}
		sort.Strings(types)
		summary := summaries[key]
		summary.ActiveItemTypes = types
		summaries[key] = summary
	}
	return summaries
}

// FormatEvent формирует стабильную человекочитаемую строку для CLI и dashboard.
// Сортировка Usage нужна для воспроизводимых тестов и удобного diff журналов.
func FormatEvent(event RuntimeEvent) string {
	parts := []string{event.Time.Local().Format(time.RFC3339), event.Kind}
	if event.StepID != "" {
		parts = append(parts, "step="+event.StepID)
	}
	if event.VisitID != "" {
		parts = append(parts, "visit="+event.VisitID)
	}
	if event.State != "" {
		parts = append(parts, "state="+event.State)
	}
	if event.ThreadID != "" {
		parts = append(parts, "thread="+event.ThreadID)
	}
	if event.TurnID != "" {
		parts = append(parts, "turn="+event.TurnID)
	}
	if event.PID != 0 {
		parts = append(parts, fmt.Sprintf("pid=%d", event.PID))
	}
	if event.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("exit=%d", *event.ExitCode))
	}
	if event.Signal != "" {
		parts = append(parts, "signal="+event.Signal)
	}
	if event.ItemType != "" {
		parts = append(parts, "item="+event.ItemType)
	}
	if len(event.Usage) != 0 {
		keys := make([]string, 0, len(event.Usage))
		for key := range event.Usage {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", key, event.Usage[key]))
		}
	}
	line := strings.Join(parts, " ")
	if event.Message != "" {
		line += " — " + event.Message
	}
	return SafeTerminalText(line)
}

// SafeTerminalText сохраняет читаемые Unicode-символы, а управляющие C0/C1 и
// разделители строк/абзацев Zl/Zp показывает как обычный текст. В частности,
// ESC больше не может начать ANSI/OSC-команду, а U+2028/U+2029 — визуально
// разорвать одну запись на несколько. Экранирование выполняется только при
// выводе: точные ID и сообщения остаются в хранилище для расследования.
func SafeTerminalText(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for _, character := range value {
		if unicode.IsControl(character) || unicode.Is(unicode.Zl, character) || unicode.Is(unicode.Zp, character) {
			fmt.Fprintf(&result, `\u%04X`, character)
			continue
		}
		result.WriteRune(character)
	}
	return result.String()
}

func normalizeRuntimeEvent(event *RuntimeEvent) error {
	if event.Time.IsZero() || !validText(event.RunID) || !validText(event.Kind) {
		return errors.New("событие требует time, runId и kind")
	}
	for _, value := range []string{event.StepID, event.VisitID, event.ThreadID, event.TurnID, event.State, event.ItemID, event.ItemType, event.Message, event.Content, event.Signal} {
		if !utf8.ValidString(value) {
			return errors.New("поля события должны быть UTF-8")
		}
	}
	event.Message = compactEventText(event.Message, 4000)
	event.Content = compactTraceText(event.Content, 16000)
	event.State = compactEventText(event.State, 200)
	event.ItemID = compactEventText(event.ItemID, 500)
	event.ItemType = compactEventText(event.ItemType, 200)
	event.Signal = compactEventText(event.Signal, 100)
	if len(event.Usage) > 64 {
		return errors.New("событие содержит слишком много счётчиков usage")
	}
	for key := range event.Usage {
		if !utf8.ValidString(key) || strings.TrimSpace(key) == "" || len([]rune(key)) > 200 {
			return errors.New("ключи usage должны быть непустыми UTF-8 строками не длиннее 200 символов")
		}
	}
	return nil
}

// compactTraceText сохраняет переносы и табуляцию, необходимые для читаемого
// stdout и сообщений в браузере. Остальные управляющие символы удаляются: JSON и
// textContent и так не исполняют их, но невидимые terminal controls не должны
// искажать сохранённое представление. Лимит относится к одному delta-событию и
// защищает append от неожиданно большого сообщения новой версии App Server.
func compactTraceText(value string, limit int) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' || !unicode.IsControl(character) {
			return character
		}
		return -1
	}, value)
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

func compactEventText(value string, limit int) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, value))
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}
