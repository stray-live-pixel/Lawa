package reviewstore

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fixture создаёт реальный каталог: тесты проверяют файловые гарантии, не моки.
func fixture(t *testing.T) (*Store, Review) {
	t.Helper()
	s := New(filepath.Join(t.TempDir(), "runs"))
	r, e := s.Create(CreateOptions{CWD: t.TempDir(), Prompt: "Проведи ревью локальных изменений"})
	if e != nil {
		t.Fatal(e)
	}
	return s, r
}

func TestCreateRoundTripAndWorkflowMarker(t *testing.T) {
	s, r := fixture(t)
	if !ValidID(r.ID) || !IsReview(s.Root, r.ID) {
		t.Fatal("не опубликован review marker")
	}
	loaded, e := s.Load(r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if loaded.Config.Review.Model != "gpt-6-astra" || loaded.Config.Context.Model != "gpt-6-sol" || loaded.Config.Presentation.Effort != "high" {
		t.Fatalf("настройки: %#v", loaded.Config)
	}
	p, e := s.ReadArtifact(r.ID, "artifacts/prompt.md")
	if e != nil || string(p) != r.Prompt {
		t.Fatalf("промпт не сохранён: %s %v", p, e)
	}
	if _, e = os.Stat(filepath.Join(s.Root, r.ID, "workflow.json")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("review не должен выдавать себя за workflow")
	}
	workflowID := newID()
	if e = os.Mkdir(filepath.Join(s.Root, workflowID), 0700); e != nil {
		t.Fatal(e)
	}
	if IsReview(s.Root, workflowID) {
		t.Fatal("чужой каталог распознан как review")
	}
	all, e := s.List()
	if e != nil || len(all) != 1 {
		t.Fatalf("List = %v, %v", all, e)
	}
}

func TestInvalidInputsAndConfig(t *testing.T) {
	s := New(t.TempDir())
	for _, prompt := range []string{"", "  ", "bad\x00"} {
		if _, e := s.Create(CreateOptions{Prompt: prompt}); e == nil {
			t.Fatalf("принят prompt %q", prompt)
		}
	}
	c := ResolveConfig(Config{Review: AgentConfig{Model: "custom", Effort: "high"}})
	if e := ValidateConfig(c); e != nil {
		t.Fatal(e)
	}
	c.Context.Effort = "invented"
	if e := ValidateConfig(c); e == nil {
		t.Fatal("принят неизвестный effort")
	}
	for _, id := range []string{"../escape", "", newID() + "/child"} {
		if _, e := s.Load(id); e == nil {
			t.Fatal("принят небезопасный ID")
		}
	}
}

func TestUpdateImmutabilityAndMonotonicRevision(t *testing.T) {
	s, r := fixture(t)
	same, e := s.Update(r.ID, func(*Review) error { return nil })
	if e != nil || same.Revision != r.Revision || !same.UpdatedAt.Equal(r.UpdatedAt) {
		t.Fatal("чтение изменило историю")
	}
	updated, e := s.Update(r.ID, func(v *Review) error { v.Title = "Проверка входа"; return nil })
	if e != nil || updated.Revision != r.Revision+1 || !updated.UpdatedAt.After(r.UpdatedAt) {
		t.Fatalf("не обновились часы: %v", e)
	}
	for _, mutate := range []func(*Review){func(v *Review) { v.Config.Review.Model = "another" }, func(v *Review) { v.Prompt = "new" }, func(v *Review) { v.ID = newID() }, func(v *Review) { v.CWD = "/another" }} {
		if _, e = s.Update(r.ID, func(v *Review) error { mutate(v); return nil }); !errors.Is(e, ErrImmutable) {
			t.Fatalf("неизменяемое поле: %v", e)
		}
	}
	frozen := time.Now().UTC()
	_, e = s.Update(r.ID, func(v *Review) error { v.Context.FrozenAt = &frozen; return nil })
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.Update(r.ID, func(v *Review) error {
		v.Context.Links = append(v.Context.Links, Link{Label: "new", URL: "https://example.test"})
		return nil
	})
	if !errors.Is(e, ErrImmutable) {
		t.Fatalf("контекст изменён после фиксации: %v", e)
	}
}

// Два независимых Store моделируют HTTP и CLI: mutex одного объекта недостаточен.
func TestConcurrentUpdatesNoLostWrites(t *testing.T) {
	s, r := fixture(t)
	const count = 30
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			other := New(s.Root)
			_, e := other.Update(r.ID, func(v *Review) error { v.Activities = append(v.Activities, Activity{ID: fmt.Sprint(i)}); return nil })
			errs <- e
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	got, e := s.Load(r.ID)
	if e != nil || len(got.Activities) != count || got.Revision != uint64(count+1) {
		t.Fatalf("потерянные обновления: %#v %v", got, e)
	}
}

func TestArtifactsImmutableAndConfined(t *testing.T) {
	s, r := fixture(t)
	path := "artifacts/context/src/login.tsx"
	if e := s.SaveArtifact(r.ID, path, []byte("first")); e != nil {
		t.Fatal(e)
	}
	before, _ := s.Load(r.ID)
	if e := s.SaveArtifact(r.ID, path, []byte("first")); e != nil {
		t.Fatal(e)
	}
	same, _ := s.Load(r.ID)
	if same.Revision != before.Revision {
		t.Fatal("повтор записи поднял ревизию")
	}
	if e := s.SaveArtifact(r.ID, path, []byte("second")); !errors.Is(e, ErrImmutable) {
		t.Fatalf("перезаписано доказательство: %v", e)
	}
	for _, bad := range []string{"review.json", "../outside", "artifacts/../review.json", "/tmp/outside", "artifacts//double", "artifacts/x/../../review.json", "artifacts\\bad", "artifacts/x\x00"} {
		if e := s.SaveArtifact(r.ID, bad, []byte("bad")); e == nil {
			t.Errorf("принят путь %q", bad)
		}
		if _, e := s.ReadArtifact(r.ID, bad); e == nil {
			t.Errorf("прочитан путь %q", bad)
		}
	}
	outside := t.TempDir()
	if e := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, filepath.Join(s.Root, r.ID, "artifacts", "link")); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ReadArtifact(r.ID, "artifacts/link/secret"); e == nil {
		t.Fatal("прочитан внешний симлинк")
	}
	if e := s.SaveArtifact(r.ID, "artifacts/link/new", []byte("bad")); e == nil {
		t.Fatal("запись через симлинк")
	}
	if e := os.Symlink(filepath.Join(s.Root, r.ID, "review.json"), filepath.Join(s.Root, r.ID, "artifacts", "internal")); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ReadArtifact(r.ID, "artifacts/internal"); e == nil {
		t.Fatal("прочитан внутренний симлинк")
	}
}

func TestExecutionLeaseRecoveryAndStop(t *testing.T) {
	s, r := fixture(t)
	lease, e := s.AcquireExecution(r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = New(s.Root).AcquireExecution(r.ID); !errors.Is(e, ErrLocked) {
		t.Fatalf("вторая execution lease: %v", e)
	}
	start := time.Now().UTC()
	_, e = s.Update(r.ID, func(v *Review) error {
		v.State = Running
		v.Stages[0].State = Running
		v.Stages[0].Attempts = []Attempt{{Number: 1, State: Running, StartedAt: start}}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	stopped, e := s.RequestStop(r.ID)
	if e != nil || !stopped.StopRequested || stopped.State != Running {
		t.Fatal("HTTP stop должен сохранить намерение, не выдумать завершение")
	}
	if e = lease.Close(); e != nil {
		t.Fatal(e)
	}
	lease, e = s.AcquireExecution(r.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	recovered, e := s.RecoverInterrupted(r.ID)
	if e != nil || recovered.State != Interrupted || recovered.Stages[0].Attempts[0].FinishedAt == nil {
		t.Fatalf("не восстановлен сбой: %#v %v", recovered, e)
	}
	again, e := s.RecoverInterrupted(r.ID)
	if e != nil || again.Revision != recovered.Revision {
		t.Fatal("повторное восстановление изменило завершённый снимок")
	}
}

func TestEventsAndViewState(t *testing.T) {
	s, r := fixture(t)
	for _, message := range []string{"начало", "чтение", "готово"} {
		if e := s.AppendEvent(r.ID, Event{Stage: ContextStage, Kind: "activity", Message: message}); e != nil {
			t.Fatal(e)
		}
	}
	events, e := s.Events(r.ID)
	if e != nil || len(events) != 3 || events[0].Message != "начало" || events[2].Message != "готово" {
		t.Fatalf("журнал: %v %v", events, e)
	}
	latest, _ := s.Load(r.ID)
	if e = s.SaveView(r.ID, ViewState{SeenRevision: latest.Revision, SelectedStage: ReviewStage}); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveView(r.ID, ViewState{SeenRevision: 1}); e != nil {
		t.Fatal(e)
	}
	view, e := s.LoadView(r.ID)
	if e != nil || view.SeenRevision != latest.Revision {
		t.Fatal("устаревший просмотр сбросил прочитанное")
	}
	after, _ := s.Load(r.ID)
	if after.Revision != latest.Revision || !after.UpdatedAt.Equal(latest.UpdatedAt) {
		t.Fatal("просмотр поднял review в истории")
	}
}

func TestListNewestUpdatedFirst(t *testing.T) {
	s, a := fixture(t)
	b, e := s.Create(CreateOptions{CWD: a.CWD, Prompt: "второе"})
	if e != nil {
		t.Fatal(e)
	}
	all, e := s.List()
	if e != nil || all[0].ID != b.ID {
		t.Fatal("неверный порядок создания")
	}
	if _, e = s.Update(a.ID, func(r *Review) error { r.Title = "обновление"; return nil }); e != nil {
		t.Fatal(e)
	}
	all, e = s.List()
	if e != nil || all[0].ID != a.ID {
		t.Fatal("неверный порядок обновления")
	}
}

// TestRefreshCrossProcessCrash доказывает жизнеспособность координатора самой
// блокировкой процесса: файл lock остаётся после SIGKILL, но больше не занят.
func TestRefreshCrossProcessCrash(t *testing.T) {
	s, r := fixture(t)
	started := time.Now().UTC()
	_, err := s.Update(r.ID, func(v *Review) error {
		v.State = Running
		v.Stages[0].State = Running
		v.Stages[0].Attempts = []Attempt{{Number: 1, State: Running, StartedAt: started}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestLeaseChildHelper$")
	child.Env = append(os.Environ(), "LAWA_REVIEW_LOCK_HELPER=1", "LAWA_REVIEW_LOCK_ROOT="+s.Root, "LAWA_REVIEW_LOCK_ID="+r.ID)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- scanner.Text()
		} else {
			ready <- ""
		}
	}()
	select {
	case text := <-ready:
		if text != "lease acquired" {
			t.Fatalf("helper не захватил блокировку: %q", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper не ответил")
	}
	live, err := s.Refresh(r.ID)
	if err != nil || live.State != Running {
		t.Fatalf("живой координатор объявлен потерянным: %s %v", live.State, err)
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	recovered, err := s.Refresh(r.ID)
	if err != nil || recovered.State != Interrupted || recovered.Stages[0].State != Interrupted || recovered.Stages[0].Attempts[0].FinishedAt == nil {
		t.Fatalf("потерянное исполнение не восстановлено: %#v %v", recovered, err)
	}
	again, err := s.Refresh(r.ID)
	if err != nil || again.Revision != recovered.Revision {
		t.Fatal("повтор GET меняет завершённую запись")
	}
}

// TestLeaseChildHelper запускается только подпроцессом теста выше.
func TestLeaseChildHelper(t *testing.T) {
	if os.Getenv("LAWA_REVIEW_LOCK_HELPER") != "1" {
		return
	}
	s := New(os.Getenv("LAWA_REVIEW_LOCK_ROOT"))
	lease, err := s.AcquireExecution(os.Getenv("LAWA_REVIEW_LOCK_ID"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	fmt.Println("lease acquired")
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
}

func TestRefreshLeavesPendingAlone(t *testing.T) {
	s, r := fixture(t)
	refreshed, err := s.Refresh(r.ID)
	if err != nil || refreshed.State != Pending || refreshed.Revision != r.Revision {
		t.Fatal("ещё не стартовавший worker принят за сбой")
	}
}

// TestPatchViewPreservesNavigation проверяет частичную запись от polling и явное
// закрытие viewer; оба действия не меняют время review и порядок истории.
func TestPatchViewPreservesNavigation(t *testing.T) {
	s, r := fixture(t)
	tour, step, stage := "tour-1", "step-2", ReviewStage
	if err := s.PatchView(r.ID, ViewPatch{SelectedTour: &tour, SelectedStep: &step, SelectedStage: &stage}); err != nil {
		t.Fatal(err)
	}
	seen := r.Revision
	if err := s.PatchView(r.ID, ViewPatch{SeenRevision: &seen}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadView(r.ID)
	if err != nil || got.SelectedTour != tour || got.SelectedStep != step || got.SelectedStage != stage || got.SeenRevision != seen {
		t.Fatalf("частичная запись стёрла навигацию: %#v %v", got, err)
	}
	before := got.UpdatedAt
	if err = s.PatchView(r.ID, ViewPatch{}); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadView(r.ID)
	if err != nil || !got.UpdatedAt.Equal(before) {
		t.Fatal("пустая запись изменила время просмотра")
	}
	clear := ""
	if err = s.PatchView(r.ID, ViewPatch{SelectedTour: &clear, SelectedStep: &clear}); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadView(r.ID)
	if err != nil || got.SelectedTour != "" || got.SelectedStep != "" || got.SelectedStage != stage || got.SeenRevision != seen {
		t.Fatalf("явное закрытие не сохранено: %#v %v", got, err)
	}
	excessive := uint64(100000)
	if err = s.PatchView(r.ID, ViewPatch{SeenRevision: &excessive}); err != nil {
		t.Fatal(err)
	}
	older := uint64(0)
	if err = s.PatchView(r.ID, ViewPatch{SeenRevision: &older}); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadView(r.ID)
	if err != nil || got.SeenRevision != r.Revision {
		t.Fatal("просмотр не ограничен сохранённой монотонной ревизией")
	}
	after, err := s.Load(r.ID)
	if err != nil || after.Revision != r.Revision || !after.UpdatedAt.Equal(r.UpdatedAt) {
		t.Fatal("просмотр изменил review")
	}
}

// TestPatchViewConcurrentFields моделирует независимые HTTP-запросы навигации и
// отметки прочтения: оба поля должны сохраниться независимо от порядка блокировок.
func TestPatchViewConcurrentFields(t *testing.T) {
	s, r := fixture(t)
	tour := "kept-tour"
	seen := r.Revision
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs <- New(s.Root).PatchView(r.ID, ViewPatch{SelectedTour: &tour}) }()
	go func() { defer wg.Done(); errs <- New(s.Root).PatchView(r.ID, ViewPatch{SeenRevision: &seen}) }()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadView(r.ID)
	if err != nil || got.SelectedTour != tour || got.SeenRevision != seen {
		t.Fatalf("потеряно параллельное обновление: %#v %v", got, err)
	}
}

func TestCreateRequiresExistingDirectory(t *testing.T) {
	root := t.TempDir()
	s := New(filepath.Join(root, "runs"))
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{"", " ", filepath.Join(root, "missing"), file} {
		if _, err := s.Create(CreateOptions{CWD: cwd, Prompt: "review"}); err == nil {
			t.Fatalf("принят cwd %q", cwd)
		}
	}
	if _, err := os.Stat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("невалидный вход создал каталог хранилища")
	}
	real := t.TempDir()
	link := filepath.Join(root, "project-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(CreateOptions{CWD: link, Prompt: "review"})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if r.CWD != canonical {
		t.Fatalf("cwd не канонизирован: %q != %q", r.CWD, canonical)
	}
}
