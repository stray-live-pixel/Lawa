//go:build darwin || linux

package runstore

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const eventsFilename = "events.jsonl"

// RuntimeEvent — нормализованное и безопасное для оператора событие Lawa.
// Оно намеренно не повторяет сырой протокол App Server: reasoning, аргументы
// команд и инструментов, diff и произвольные payload могут содержать секреты.
// Usage содержит только числовые счётчики токенов без исходного JSON.
type RuntimeEvent struct {
	Time     time.Time        `json:"time"`
	RunID    string           `json:"runId"`
	StepID   string           `json:"stepId,omitempty"`
	ThreadID string           `json:"threadId,omitempty"`
	TurnID   string           `json:"turnId,omitempty"`
	Kind     string           `json:"kind"`
	State    string           `json:"state,omitempty"`
	ItemType string           `json:"itemType,omitempty"`
	Message  string           `json:"message,omitempty"`
	PID      int              `json:"pid,omitempty"`
	ExitCode *int             `json:"exitCode,omitempty"`
	Signal   string           `json:"signal,omitempty"`
	Usage    map[string]int64 `json:"usage,omitempty"`
}

// EventSummary сворачивает журнал в последние полезные поля одного кубика.
// Это производное read-only представление; meta.json остаётся источником истины
// для планировщика, а events.jsonl — источником диагностики оператора.
type EventSummary struct {
	StepID, ThreadID, TurnID, Kind, State, Message string
	LastActivity                                   time.Time
	PID                                            int
	ExitCode                                       *int
	Signal                                         string
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
	if event.StepID != "" {
		known := false
		for _, step := range snapshot.Meta.Steps {
			known = known || step.ID == event.StepID
		}
		if !known {
			return fmt.Errorf("событие ссылается на неизвестный шаг %q", event.StepID)
		}
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
	dir, err := openRun(root, runID)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if _, err = loadForDashboard(dir, runID); err != nil {
		return nil, err
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
		var event RuntimeEvent
		if err = json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("%s, строка %d: %w", eventsFilename, line, err)
		}
		if event.RunID != runID {
			return nil, fmt.Errorf("%s, строка %d: неверный runId", eventsFilename, line)
		}
		if err = normalizeRuntimeEvent(&event); err != nil {
			return nil, fmt.Errorf("%s, строка %d: %w", eventsFilename, line, err)
		}
		events = append(events, event)
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// SummarizeEvents возвращает по одному последнему снимку на кубик. PID остаётся
// активным только между process_started и process_exited этого же процесса.
func SummarizeEvents(events []RuntimeEvent) map[string]EventSummary {
	summaries := make(map[string]EventSummary)
	for _, event := range events {
		if event.StepID == "" {
			continue
		}
		summary := summaries[event.StepID]
		summary.StepID = event.StepID
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
		case "process_exited":
			summary.PID, summary.ExitCode, summary.Signal = 0, event.ExitCode, event.Signal
		}
		summaries[event.StepID] = summary
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
	return line
}

func normalizeRuntimeEvent(event *RuntimeEvent) error {
	if event.Time.IsZero() || !validText(event.RunID) || !validText(event.Kind) {
		return errors.New("событие требует time, runId и kind")
	}
	for _, value := range []string{event.StepID, event.ThreadID, event.TurnID, event.State, event.ItemType, event.Message, event.Signal} {
		if !utf8.ValidString(value) {
			return errors.New("поля события должны быть UTF-8")
		}
	}
	event.Message = compactEventText(event.Message, 4000)
	event.State = compactEventText(event.State, 200)
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
