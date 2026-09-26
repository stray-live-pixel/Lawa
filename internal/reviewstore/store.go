//go:build darwin || linux

package reviewstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
)

var ErrLocked = errors.New("ревью уже исполняется другим процессом")
var ErrImmutable = errors.New("неизменяемые данные ревью нельзя перезаписать")

// Store работает поверх локальной файловой системы. Root задаёт доверенный каталог,
// а все пути агентов ограничены os.Root выбранного ревью. Значение можно разделять
// между HTTP и CLI: короткие flock сериализуют все изменения между процессами.
type Store struct{ Root string }

// New создаёт дескриптор; каталог создаётся только при Create.
func New(root string) *Store { return &Store{Root: root} }

// DefaultConfig задаёт согласованные модели и датированный базовый API-эквивалент.
// Неизвестные модели не получают предполагаемый тариф; надбавки не симулируются.
func DefaultConfig() Config {
	prices := map[string]Pricing{}
	for model, values := range map[string][3]float64{
		"gpt-6-astra": {10, 1, 50},
		"gpt-6-sol":   {2, .2, 10},
		"gpt-6-luna":  {.1, .01, .5},
	} {
		prices[model] = Pricing{Input: values[0], CachedInput: values[1], Output: values[2], Source: "https://developers.openai.com/api/docs/pricing", AsOf: "2026-09-26", Basis: "Standard short-context token equivalent; excludes cache writes, tools, regional and fast surcharges"}
	}
	return Config{Context: AgentConfig{"gpt-6-sol", "high"}, Review: AgentConfig{"gpt-6-astra", "high"}, Presentation: AgentConfig{"gpt-6-sol", "high"}, RublesPerDollar: 100, Prices: prices}
}

// ResolveConfig дополняет частичные настройки до неизменяемого снимка запуска.
func ResolveConfig(c Config) Config {
	d := DefaultConfig()
	for _, p := range []struct {
		v *AgentConfig
		d AgentConfig
	}{{&c.Context, d.Context}, {&c.Review, d.Review}, {&c.Presentation, d.Presentation}} {
		if p.v.Model == "" {
			p.v.Model = p.d.Model
		}
		if p.v.Effort == "" {
			p.v.Effort = p.d.Effort
		}
	}
	if c.RublesPerDollar == 0 {
		c.RublesPerDollar = d.RublesPerDollar
	}
	c.Prices = MergeConfig(d, Config{Prices: c.Prices}).Prices
	return c
}

// Create публикует полностью готовую папку rename-ом, поэтому List никогда не
// видит полуготовое ревью. meta.json позволяет соседним сканерам отличить review.
func (s *Store) Create(o CreateOptions) (Review, error) {
	if !validText(o.CWD) {
		return Review{}, errors.New("нужна рабочая папка проекта")
	}
	cwd, err := filepath.Abs(o.CWD)
	if err != nil {
		return Review{}, err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return Review{}, fmt.Errorf("рабочая папка проекта: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return Review{}, err
	}
	if !info.IsDir() {
		return Review{}, errors.New("рабочая папка проекта должна быть каталогом")
	}
	now := time.Now().UTC()
	r := Review{Version: 1, Kind: "review", ID: newID(), CWD: cwd, Prompt: o.Prompt, CreatedAt: now, UpdatedAt: now, Revision: 1, State: Pending, CurrentStage: ContextStage, Config: ResolveConfig(o.Config)}
	for _, id := range []StageID{ContextStage, ReviewStage, PresentationStage} {
		r.Stages = append(r.Stages, Stage{ID: id, State: Pending, Attempts: []Attempt{}})
	}
	if err = Validate(r); err != nil {
		return Review{}, err
	}
	if err = os.MkdirAll(s.Root, 0700); err != nil {
		return Review{}, err
	}
	tmp, err := os.MkdirTemp(s.Root, ".review-")
	if err != nil {
		return Review{}, err
	}
	defer os.RemoveAll(tmp)
	dir, err := os.OpenRoot(tmp)
	if err != nil {
		return Review{}, err
	}
	defer dir.Close()
	for _, name := range []string{"artifacts", "events"} {
		if err = dir.Mkdir(name, 0700); err != nil {
			return Review{}, err
		}
	}
	// Создаём lock inode до публикации: параллельные открытия не создают его заново.
	for _, name := range []string{"store.lock", "execution.lock"} {
		if err = atomicWrite(dir, name, nil); err != nil {
			return Review{}, err
		}
	}
	if err = atomicJSON(dir, "meta.json", struct {
		Kind    string `json:"kind"`
		Version int    `json:"version"`
	}{"review", 1}); err != nil {
		return Review{}, err
	}
	if err = atomicJSON(dir, "review.json", r); err != nil {
		return Review{}, err
	}
	if err = atomicWrite(dir, "artifacts/prompt.md", []byte(o.Prompt)); err != nil {
		return Review{}, err
	}
	if err = os.Rename(tmp, filepath.Join(s.Root, r.ID)); err != nil {
		return Review{}, err
	}
	if err = syncPath(s.Root); err != nil {
		return Review{}, err
	}
	return r, nil
}

// Load возвращает независимый снимок; атомарный rename исключает частичный JSON.
func (s *Store) Load(id string) (Review, error) {
	d, e := s.open(id)
	if e != nil {
		return Review{}, e
	}
	defer d.Close()
	return load(d, id)
}

// List пропускает чужие workflow и временные каталоги, но сообщает повреждение
// настоящего review вместо молчаливого исчезновения записи из истории.
func (s *Store) List() ([]Review, error) {
	entries, e := os.ReadDir(s.Root)
	if errors.Is(e, os.ErrNotExist) {
		return []Review{}, nil
	}
	if e != nil {
		return nil, e
	}
	result := []Review{}
	for _, entry := range entries {
		if !entry.IsDir() || !ValidID(entry.Name()) {
			continue
		}
		d, e := s.open(entry.Name())
		if e != nil {
			return nil, e
		}
		info, e := d.Lstat("review.json")
		d.Close()
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("review.json должен быть обычным файлом")
		}
		r, e := s.Load(entry.Name())
		if e != nil {
			return nil, fmt.Errorf("ревью %s: %w", entry.Name(), e)
		}
		result = append(result, r)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

// Update заново читает снимок под блокировкой, применяет callback и проверяет
// инварианты. Callback не должен вызывать другие методы записи того же review.
// Конфигурация, исходная постановка и уже замороженный контекст неизменяемы.
func (s *Store) Update(id string, fn func(*Review) error) (Review, error) {
	d, l, e := s.lock(id, "store.lock", false)
	if e != nil {
		return Review{}, e
	}
	defer d.Close()
	defer l.Close()
	r, e := load(d, id)
	if e != nil {
		return Review{}, e
	}
	before, e := json.Marshal(r)
	if e != nil {
		return Review{}, e
	}
	var original Review
	if e = json.Unmarshal(before, &original); e != nil {
		return Review{}, e
	}
	if e = fn(&r); e != nil {
		return Review{}, e
	}
	if r.ID != original.ID || r.Kind != original.Kind || r.Version != original.Version || r.CWD != original.CWD || r.Prompt != original.Prompt || !r.CreatedAt.Equal(original.CreatedAt) || !reflect.DeepEqual(r.Config, original.Config) || original.Context.FrozenAt != nil && !reflect.DeepEqual(r.Context, original.Context) {
		return Review{}, ErrImmutable
	}
	r.UpdatedAt = original.UpdatedAt
	r.Revision = original.Revision
	// Порядок ключей map при JSON-кодировании не определён: сравниваем значения,
	// иначе неизменившиеся тарифы создавали бы ложные обновления истории.
	if reflect.DeepEqual(r, original) {
		return r, nil
	}
	r.UpdatedAt = nextTime(original.UpdatedAt)
	r.Revision++
	if e = Validate(r); e != nil {
		return Review{}, e
	}
	if e = atomicJSON(d, "review.json", r); e != nil {
		return Review{}, e
	}
	return r, nil
}

// RequestStop сохраняет намерение; исполнитель прерывает текущего агента и
// публикует окончательное состояние, не теряя уже накопленные материалы.
func (s *Store) RequestStop(id string) (Review, error) {
	return s.Update(id, func(r *Review) error {
		if r.State == Running || r.State == Pending {
			r.StopRequested = true
		}
		return nil
	})
}

// SaveArtifact сохраняет неизменяемый материал. Повтор тех же байтов безопасен;
// новые версии должны иметь новый путь, чтобы старые доказательства не менялись.
func (s *Store) SaveArtifact(id, path string, data []byte) error {
	if e := validArtifact(path); e != nil {
		return e
	}
	d, l, e := s.lock(id, "store.lock", false)
	if e != nil {
		return e
	}
	defer d.Close()
	defer l.Close()
	r, e := load(d, id)
	if e != nil {
		return e
	}
	if e = noSymlinks(d, path, true); e != nil {
		return e
	}
	existing, e := d.ReadFile(path)
	if e == nil {
		if string(existing) == string(data) {
			return nil
		}
		return ErrImmutable
	}
	if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if e = mkdirParents(d, filepath.Dir(path)); e != nil {
		return e
	}
	if e = atomicWrite(d, path, data); e != nil {
		return e
	}
	r.UpdatedAt = nextTime(r.UpdatedAt)
	r.Revision++
	return atomicJSON(d, "review.json", r)
}

// ReadArtifact не позволяет агентскому пути выйти из review и запрещает симлинки.
func (s *Store) ReadArtifact(id, path string) ([]byte, error) {
	if e := validArtifact(path); e != nil {
		return nil, e
	}
	d, e := s.open(id)
	if e != nil {
		return nil, e
	}
	defer d.Close()
	if e := noSymlinks(d, path, false); e != nil {
		return nil, e
	}
	return d.ReadFile(path)
}

// AppendEvent сохраняет отдельный JSON атомарно: сбой посреди записи не портит
// журнал. Событие увеличивает ревизию, чтобы UI отмечал новые действия агента.
func (s *Store) AppendEvent(id string, event Event) error {
	d, l, e := s.lock(id, "store.lock", false)
	if e != nil {
		return e
	}
	defer d.Close()
	defer l.Close()
	r, e := load(d, id)
	if e != nil {
		return e
	}
	event.ID = newID()
	event.At = nextTime(r.UpdatedAt)
	name := fmt.Sprintf("events/%020d-%s.json", r.Revision+1, event.ID)
	if e = atomicJSON(d, name, event); e != nil {
		return e
	}
	r.UpdatedAt = event.At
	r.Revision++
	return atomicJSON(d, "review.json", r)
}

// Events возвращает журнал в порядке записи, включая последнюю сохранённую
// запись после возможного отказа обновления review.json.
func (s *Store) Events(id string) ([]Event, error) {
	d, e := s.open(id)
	if e != nil {
		return nil, e
	}
	defer d.Close()
	f, e := d.Open("events")
	if e != nil {
		return nil, e
	}
	entries, e := f.ReadDir(-1)
	f.Close()
	if e != nil {
		return nil, e
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := []Event{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := "events/" + entry.Name()
		if e := noSymlinks(d, path, false); e != nil {
			return nil, e
		}
		b, e := d.ReadFile(path)
		if e != nil {
			return nil, e
		}
		var event Event
		if e = json.Unmarshal(b, &event); e != nil {
			return nil, e
		}
		result = append(result, event)
	}
	return result, nil
}

// LoadView не создаёт файлов и не меняет время самого review.
func (s *Store) LoadView(id string) (ViewState, error) {
	d, e := s.open(id)
	if e != nil {
		return ViewState{}, e
	}
	defer d.Close()
	b, e := readRegular(d, "view.json")
	if errors.Is(e, os.ErrNotExist) {
		return ViewState{}, nil
	}
	if e != nil {
		return ViewState{}, e
	}
	var v ViewState
	e = json.Unmarshal(b, &v)
	return v, e
}

// SaveView заменяет все поля навигации, но не позволяет уменьшить просмотренную
// ревизию. Частичные HTTP-обновления должны использовать PatchView.
func (s *Store) SaveView(id string, v ViewState) error {
	return s.PatchView(id, ViewPatch{SeenRevision: &v.SeenRevision, SelectedStage: &v.SelectedStage, SelectedTour: &v.SelectedTour, SelectedStep: &v.SelectedStep})
}

// PatchView применяет только переданные поля под одной блокировкой. Polling
// отметки прочтения не стирает одновременно выбранную экскурсию или этап.
func (s *Store) PatchView(id string, patch ViewPatch) error {
	if patch.SelectedStage != nil && *patch.SelectedStage != "" && !validStage(*patch.SelectedStage) {
		return errors.New("неизвестный этап просмотра")
	}
	d, l, e := s.lock(id, "store.lock", false)
	if e != nil {
		return e
	}
	defer d.Close()
	defer l.Close()
	r, e := load(d, id)
	if e != nil {
		return e
	}
	b, e := readRegular(d, "view.json")
	var v ViewState
	if e == nil {
		if e = json.Unmarshal(b, &v); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	before := v
	if patch.SeenRevision != nil {
		seen := *patch.SeenRevision
		if seen > r.Revision {
			seen = r.Revision
		}
		if seen > v.SeenRevision {
			v.SeenRevision = seen
		}
	}
	if patch.SelectedStage != nil {
		v.SelectedStage = *patch.SelectedStage
	}
	if patch.SelectedTour != nil {
		v.SelectedTour = *patch.SelectedTour
	}
	if patch.SelectedStep != nil {
		v.SelectedStep = *patch.SelectedStep
	}
	if v == before {
		return nil
	}
	v.UpdatedAt = time.Now().UTC()
	return atomicJSON(d, "view.json", v)
}

// AcquireExecution удерживает отдельную блокировку координатора до Close.
// Она не мешает HTTP читать, останавливать и сохранять состояние просмотра.
func (s *Store) AcquireExecution(id string) (io.Closer, error) {
	d, l, e := s.lock(id, "execution.lock", true)
	if e != nil {
		return nil, e
	}
	if _, e = load(d, id); e != nil {
		l.Close()
		d.Close()
		return nil, e
	}
	d.Close()
	return l, nil
}

// RecoverInterrupted вызывается только после AcquireExecution: живой процесс
// нельзя объявлять потерянным по одному времени в файле. Повтор требует явного
// запуска новой попытки и не повторяет возможную внешнюю публикацию автоматически.
func (s *Store) RecoverInterrupted(id string) (Review, error) {
	return s.Update(id, func(r *Review) error {
		if r.State != Running {
			return nil
		}
		r.State = Interrupted
		r.Error = "Предыдущее исполнение прервано; доступен повтор этапа"
		now := time.Now().UTC()
		for i := range r.Stages {
			st := &r.Stages[i]
			if st.State == Running {
				st.State = Interrupted
				for j := range st.Attempts {
					a := &st.Attempts[j]
					if a.State == Running {
						a.State = Interrupted
						a.FinishedAt = &now
					}
				}
			}
		}
		return nil
	})
}

// open ограничивает разрешение путей выбранным каталогом и проверяет ID.
func (s *Store) open(id string) (*os.Root, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("некорректный reviewId %q", id)
	}
	base, e := os.OpenRoot(s.Root)
	if e != nil {
		return nil, e
	}
	defer base.Close()
	info, e := base.Lstat(id)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("review должен быть каталогом")
	}
	return base.OpenRoot(id)
}

// lock использует неизменяемый inode: lock-файлы никогда не удаляются.
func (s *Store) lock(id, name string, nonblock bool) (*os.Root, *os.File, error) {
	d, e := s.open(id)
	if e != nil {
		return nil, nil, e
	}
	fail := func(e error) (*os.Root, *os.File, error) { d.Close(); return nil, nil, e }
	if e := noSymlinks(d, name, true); e != nil {
		return fail(e)
	}
	l, e := d.OpenFile(name, os.O_RDWR|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return fail(e)
	}
	info, e := l.Stat()
	if e != nil || !info.Mode().IsRegular() {
		l.Close()
		return fail(fmt.Errorf("неверный lock-файл: %v", e))
	}
	flags := syscall.LOCK_EX
	if nonblock {
		flags |= syscall.LOCK_NB
	}
	if e = syscall.Flock(int(l.Fd()), flags); e != nil {
		l.Close()
		if errors.Is(e, syscall.EWOULDBLOCK) {
			e = ErrLocked
		}
		return fail(e)
	}
	return d, l, nil
}

// load проверяет формат целиком после чтения атомарного снимка.
func load(d *os.Root, id string) (Review, error) {
	b, e := readRegular(d, "review.json")
	if e != nil {
		return Review{}, e
	}
	var r Review
	if e = json.Unmarshal(b, &r, json.RejectUnknownMembers(true)); e != nil {
		return Review{}, e
	}
	if r.ID != id {
		return Review{}, fmt.Errorf("reviewId не совпадает с каталогом")
	}
	return r, Validate(r)
}

// atomicJSON не оставляет частично записанный JSON видимым читателям.
func atomicJSON(d *os.Root, name string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return atomicWrite(d, name, b)
}

// atomicWrite синхронизирует файл и родительский каталог после rename.
func atomicWrite(d *os.Root, name string, b []byte) error {
	if e := noSymlinks(d, name, true); e != nil {
		return e
	}
	tmp := filepath.Join(filepath.Dir(name), ".write-"+newID())
	f, e := d.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer d.Remove(tmp)
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if e = d.Rename(tmp, name); e != nil {
		return e
	}
	parent, e := d.Open(filepath.Dir(name))
	if e != nil {
		return e
	}
	return errors.Join(parent.Sync(), parent.Close())
}

// mkdirParents создаёт только обычные подкаталоги внутри уже открытого root.
func mkdirParents(d *os.Root, path string) error {
	parts := strings.Split(filepath.ToSlash(path), "/")
	cur := ""
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		info, e := d.Lstat(cur)
		if errors.Is(e, os.ErrNotExist) {
			if e = d.Mkdir(cur, 0700); e != nil {
				return e
			}
			parent, e := d.Open(filepath.Dir(cur))
			if e != nil {
				return e
			}
			if e = errors.Join(parent.Sync(), parent.Close()); e != nil {
				return e
			}
			continue
		}
		if e != nil {
			return e
		}
		if !info.IsDir() {
			return fmt.Errorf("%s должен быть каталогом", cur)
		}
	}
	return nil
}

// noSymlinks запрещает перенаправления и специальные файлы; os.Root дополнительно
// защищает от выхода за пределы каталога при конкурентной подмене пути.
func noSymlinks(d *os.Root, path string, missing bool) error {
	cur := ""
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i, p := range parts {
		cur = filepath.Join(cur, p)
		info, e := d.Lstat(cur)
		if errors.Is(e, os.ErrNotExist) && missing {
			return nil
		}
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("симлинки запрещены: %s", path)
		}
		if i < len(parts)-1 {
			if !info.IsDir() {
				return fmt.Errorf("родитель не каталог: %s", cur)
			}
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("ожидался обычный файл: %s", cur)
		}
	}
	return nil
}
func readRegular(d *os.Root, path string) ([]byte, error) {
	if e := noSymlinks(d, path, false); e != nil {
		return nil, e
	}
	return d.ReadFile(path)
}
func syncPath(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	return errors.Join(f.Sync(), f.Close())
}
func nextTime(previous time.Time) time.Time {
	now := time.Now().UTC()
	if !now.After(previous) {
		return previous.Add(time.Nanosecond)
	}
	return now
}
func newID() string { var b [16]byte; rand.Read(b[:]); return hex.EncodeToString(b[:]) }

// ValidID проверяет непрозрачный идентификатор без интерпретации как пути.
func ValidID(id string) bool {
	_, e := hex.DecodeString(id)
	return len(id) == 32 && e == nil && id == strings.ToLower(id)
}

// IsReview читает только marker, чтобы workflow-сканер не пытался загрузить
// review как workflow. Отсутствующий каталог/marker означает другой тип запуска.
func IsReview(root, id string) bool {
	s := New(root)
	d, e := s.open(id)
	if e != nil {
		return false
	}
	defer d.Close()
	b, e := readRegular(d, "meta.json")
	if e != nil {
		return false
	}
	var m struct {
		Kind string `json:"kind"`
	}
	return json.Unmarshal(b, &m) == nil && m.Kind == "review"
}

// Refresh возвращает актуальный снимок и обнаруживает потерянного координатора.
// Только свободная execution.lock доказывает, что Running больше никто не ведёт:
// время обновления и оставшийся lock-файл сами по себе не являются доказательством.
// Pending не восстанавливается: HTTP уже мог создать ещё не стартовавший worker.
func (s *Store) Refresh(id string) (Review, error) {
	r, err := s.Load(id)
	if err != nil || r.State != Running {
		return r, err
	}
	lease, err := s.AcquireExecution(id)
	if errors.Is(err, ErrLocked) {
		return s.Load(id)
	}
	if err != nil {
		return Review{}, err
	}
	// Владея execution lease, повторно читаем данные внутри Update: исполнитель
	// мог успеть завершиться между первым Load и захватом блокировки.
	r, err = s.RecoverInterrupted(id)
	return r, errors.Join(err, lease.Close())
}
