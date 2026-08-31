//go:build darwin || linux

// Package series хранит состояние повторяющегося запуска и сериализует решение
// «создать следующий run или остановиться». Сами run остаются в runstore: серия
// содержит только расписание, прогресс и ссылку на текущий обычный запуск.
package series

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

// Mode определяет момент следующего запуска. Во всех режимах первый
// последовательный run начинается сразу, а cron ждёт первой календарной точки.
type Mode string

const (
	Immediate Mode = "immediate"
	After     Mode = "after"
	Cron      Mode = "cron"
)

// State описывает жизненный цикл серии, а не состояние отдельного workflow.
type State string

const (
	Waiting   State = "waiting"
	Running   State = "running"
	Stopped   State = "stopped"
	Completed State = "completed"
	Failed    State = "failed"
)

// Config — сохранённый контракт повторения. Delay хранится строкой, чтобы
// series.json оставался понятным человеку и не зависел от наносекунд JSON.
type Config struct {
	Mode     Mode   `json:"mode"`
	Delay    string `json:"delay,omitempty"`
	Cron     string `json:"cron,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
	MaxRuns  int    `json:"maxRuns,omitempty"`
}

// Metadata позволяет оператору понять режим, прогресс и ближайшее действие.
// RunsFinished включает успешные и неуспешные терминальные run; LastError
// объясняет, почему политика stop-on-failure остановила серию.
type Metadata struct {
	Version      int        `json:"version"`
	SeriesID     string     `json:"seriesId"`
	Config       Config     `json:"config"`
	State        State      `json:"state"`
	RunsStarted  int        `json:"runsStarted"`
	RunsFinished int        `json:"runsFinished"`
	CurrentRunID string     `json:"currentRunId,omitempty"`
	NextRunAt    *time.Time `json:"nextRunAt,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
}

// Snapshot дополняет атомарные метаданные отдельным stop-маркером. Маркер не
// переписывает series.json из второго процесса и поэтому не может потерять
// конкурентное обновление владельца серии.
type Snapshot struct {
	Metadata
	StopRequested bool
}

// Schedule скрывает конкретный cron-парсер от CLI и тестов управляемых часов.
type Schedule interface {
	Next(now time.Time, runsStarted int) time.Time
}

type schedule struct {
	config Config
	delay  time.Duration
	cron   cron.Schedule
}

// ParseConfig проверяет совместимость CLI-флагов и заранее компилирует cron.
// Поддерживается стандартный пятичастный синтаксис: minute hour day month weekday.
func ParseConfig(mode, delay, expression, zone, maxRuns string) (Config, Schedule, error) {
	config := Config{Mode: Mode(mode), Delay: delay, Cron: expression, TimeZone: zone}
	if maxRuns != "" {
		limit, err := strconv.Atoi(maxRuns)
		if err != nil || limit <= 0 {
			return Config{}, nil, errors.New("--max-runs должен быть положительным целым числом")
		}
		config.MaxRuns = limit
	}
	parsed := &schedule{config: config}
	switch config.Mode {
	case Immediate:
		if delay != "" || expression != "" || zone != "" {
			return Config{}, nil, errors.New("repeat=immediate не принимает --repeat-delay, --cron или --timezone")
		}
	case After:
		if expression != "" || zone != "" {
			return Config{}, nil, errors.New("repeat=after принимает --repeat-delay, но не --cron/--timezone")
		}
		var err error
		if parsed.delay, err = time.ParseDuration(delay); err != nil || parsed.delay <= 0 {
			return Config{}, nil, errors.New("repeat=after требует положительный --repeat-delay, например 1h")
		}
	case Cron:
		if delay != "" || strings.TrimSpace(expression) == "" || strings.TrimSpace(zone) == "" {
			return Config{}, nil, errors.New("repeat=cron требует --cron и --timezone и не принимает --repeat-delay")
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return Config{}, nil, fmt.Errorf("неизвестная временная зона %q: %w", zone, err)
		}
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		var err error
		parsed.cron, err = parser.Parse("CRON_TZ=" + zone + " " + expression)
		if err != nil {
			return Config{}, nil, fmt.Errorf("неверное cron-расписание: %w", err)
		}
	default:
		return Config{}, nil, errors.New("--repeat должен быть immediate, after или cron")
	}
	return config, parsed, nil
}

// Next возвращает первую допустимую точку после переданного состояния. Для
// after вызывающий передаёт время завершения предыдущего run, а не его старта.
func (s *schedule) Next(now time.Time, runsStarted int) time.Time {
	switch s.config.Mode {
	case Immediate:
		return now
	case After:
		if runsStarted == 0 {
			return now
		}
		return now.Add(s.delay)
	default:
		// cron.Schedule.Next всегда выбирает точку строго после now. Поэтому
		// завершившийся долгий run не создаёт очередь пропущенных событий.
		return s.cron.Next(now)
	}
}

// WaitUntil ждёт календарную точку короткими отрезками, чтобы отдельная команда
// stop была замечена без ожидания далёкого таймера. now передаётся зависимостью:
// тесты продвигают часы без sleep.
func WaitUntil(ctx context.Context, target time.Time, now func() time.Time, stopped func() (bool, error)) error {
	for now().Before(target) {
		requested, err := stopped()
		if err != nil || requested {
			if requested && err == nil {
				return ErrStopped
			}
			return err
		}
		pause := target.Sub(now())
		if pause > time.Second {
			pause = time.Second
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// ErrStopped — штатный результат stop-маркера, а не ошибка серии.
var ErrStopped = errors.New("серия остановлена пользователем")

// LockedSeries удерживает единственного владельца цикла. launch.lock имеет
// более короткую область: RequestStop берёт его перед публикацией stop-маркера,
// поэтому запуск и остановка получают однозначный порядок между процессами.
type LockedSeries struct {
	mu   sync.Mutex
	dir  string
	meta Metadata
	lock *os.File
}

// Create создаёт приватный каталог серии, публикует начальное состояние и сразу
// захватывает пожизненную блокировку владельца. Частично созданный каталог удаляется.
func Create(root string, config Config) (_ *LockedSeries, err error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("нужна папка хранения root")
	}
	base := filepath.Join(root, "series")
	if err = os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	owner := &LockedSeries{meta: Metadata{Version: 1, SeriesID: newID(), Config: config, State: Waiting}}
	owner.dir = filepath.Join(base, owner.meta.SeriesID)
	if err = os.Mkdir(owner.dir, 0o700); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, owner.Close(), os.RemoveAll(owner.dir))
		}
	}()
	if err = owner.save(); err != nil {
		return nil, err
	}
	owner.lock, err = lockFile(filepath.Join(owner.dir, "coordinator.lock"), true)
	if err != nil {
		return nil, err
	}
	return owner, nil
}

// Load читает целостный снимок для series-status без захвата блокировки владельца.
func Load(root, seriesID string) (Snapshot, error) {
	dir, err := seriesDir(root, seriesID)
	if err != nil {
		return Snapshot{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "series.json"))
	if err != nil {
		return Snapshot{}, err
	}
	var meta Metadata
	if err = json.Unmarshal(data, &meta); err != nil {
		return Snapshot{}, err
	}
	if meta.Version != 1 || meta.SeriesID != seriesID {
		return Snapshot{}, errors.New("повреждены метаданные серии")
	}
	_, err = os.Stat(filepath.Join(dir, "stop"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, err
	}
	return Snapshot{Metadata: meta, StopRequested: err == nil}, nil
}

// RequestStop сериализуется с StartRun. Если новый run уже прошёл барьер, он
// считается текущим и спокойно завершается; все более поздние запуски запрещены.
func RequestStop(root, seriesID string) error {
	return requestStop(root, seriesID, false)
}

// requestStop принимает режим блокировки только для детерминированной проверки
// занятого launch.lock. Публичная команда всегда ждёт освобождения этого барьера.
func requestStop(root, seriesID string, nonBlocking bool) error {
	dir, err := seriesDir(root, seriesID)
	if err != nil {
		return err
	}
	guard, err := lockFile(filepath.Join(dir, "launch.lock"), nonBlocking)
	if err != nil {
		return err
	}
	defer guard.Close()
	f, err := os.OpenFile(filepath.Join(dir, "stop"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return syncDirectory(dir)
}

// SetNext сохраняет видимое время следующего запуска до ожидания.
func (s *LockedSeries) SetNext(next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta.State, s.meta.NextRunAt = Waiting, &next
	return s.save()
}

// StartRun выполняет создание обычного run внутри межпроцессного барьера. Если
// связь с серией не опубликована, новый и ещё никому не переданный run удаляется.
// false означает, что stop победил гонку либо создание было полностью отменено.
func (s *LockedSeries) StartRun(create func() (string, error), rollback func(string) error) (bool, error) {
	return s.startRun(create, rollback, s.savePublished)
}

func (s *LockedSeries) startRun(create func() (string, error), rollback func(string) error, save func() (bool, error)) (bool, error) {
	if create == nil || rollback == nil {
		return false, errors.New("создание run требует функции создания и безопасного отката")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	guard, err := lockFile(filepath.Join(s.dir, "launch.lock"), false)
	if err != nil {
		return false, err
	}
	defer guard.Close()
	if requested, err := s.stopRequested(); err != nil || requested {
		return false, err
	}
	if s.meta.CurrentRunID != "" {
		return false, fmt.Errorf("run %s уже активен в серии", s.meta.CurrentRunID)
	}
	previous := s.meta
	runID, err := create()
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(runID) == "" {
		return false, errors.New("созданный run должен содержать runId")
	}
	s.meta.RunsStarted++
	s.meta.CurrentRunID, s.meta.NextRunAt, s.meta.State = runID, nil, Running
	published, saveErr := save()
	if saveErr == nil {
		return true, nil
	}
	if published {
		return true, fmt.Errorf("связь серии с run %q опубликована, но не синхронизирована: %w", runID, saveErr)
	}
	s.meta = previous
	cleanupErr := rollback(runID)
	return false, fmt.Errorf("не удалось связать run %q с серией; откат нового run: %w", runID, errors.Join(saveErr, cleanupErr))
}

// FinishRun фиксирует подтверждённый терминал обычного run. При ошибке самого
// workflow серия больше не планируется: это явная политика stop-on-failure для
// failed и interrupted. Ошибки управляющего процесса сюда передавать нельзя:
// они не доказывают терминальность run и должны сохраняться через FailRunControl.
func (s *LockedSeries) FinishRun(runErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta.RunsFinished++
	s.meta.CurrentRunID, s.meta.NextRunAt = "", nil
	if runErr != nil {
		s.meta.State, s.meta.LastError = Failed, runErr.Error()
	} else {
		s.meta.State, s.meta.LastError = Waiting, ""
	}
	return s.save()
}

// FailRunControl останавливает серию после ошибки управляющего процесса, когда
// терминальность текущего run не подтверждена. Ссылка на run и счётчик
// RunsFinished намеренно сохраняются: оператор сможет найти незавершённую работу
// через series-status и безопасно решить, продолжать ли её командой resume.
func (s *LockedSeries) FailRunControl(runErr error) error {
	if runErr == nil {
		return errors.New("ошибка управления серией не задана")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta.State, s.meta.NextRunAt, s.meta.LastError = Failed, nil, runErr.Error()
	return s.save()
}

// FinishSeries публикует штатный терминал после лимита или stop.
func (s *LockedSeries) FinishSeries(state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta.State, s.meta.NextRunAt = state, nil
	return s.save()
}

// Snapshot возвращает копию прогресса владельцу цикла.
func (s *LockedSeries) Snapshot() Metadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta
}

// StopRequested проверяет только marker и безопасен как callback ожидания.
func (s *LockedSeries) StopRequested() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopRequested()
}

func (s *LockedSeries) stopRequested() (bool, error) {
	_, err := os.Stat(filepath.Join(s.dir, "stop"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Close освобождает lock; файлы истории остаются доступны series-status.
func (s *LockedSeries) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

func (s *LockedSeries) save() error {
	_, err := s.savePublished()
	return err
}

// savePublished отличает отказ до атомарного Rename от ошибки Sync каталога.
// После Rename откат run уже небезопасен: series.json может содержать его ID.
func (s *LockedSeries) savePublished() (bool, error) {
	data, err := json.Marshal(s.meta)
	if err != nil {
		return false, err
	}
	tmp := filepath.Join(s.dir, ".series-"+newID()+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return false, err
	}
	if err = os.Rename(tmp, filepath.Join(s.dir, "series.json")); err != nil {
		return false, err
	}
	return true, syncDirectory(s.dir)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func seriesDir(root, seriesID string) (string, error) {
	if len(seriesID) != 32 {
		return "", errors.New("неверный series-id")
	}
	if _, err := hex.DecodeString(seriesID); err != nil {
		return "", errors.New("неверный series-id")
	}
	return filepath.Join(root, "series", seriesID), nil
}

func lockFile(path string, nonBlocking bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	operation := syscall.LOCK_EX
	if nonBlocking {
		operation |= syscall.LOCK_NB
	}
	if err = syscall.Flock(int(f.Fd()), operation); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(raw[:])
}
