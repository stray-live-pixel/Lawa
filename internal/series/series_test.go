package series

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func ignoreRollback(string) error { return nil }

// TestSchedulesWithControlledClock фиксирует календарную семантику без sleep:
// after отсчитывается от переданного завершения, а cron пропускает старые точки.
func TestSchedulesWithControlledClock(t *testing.T) {
	moscowMorning := time.Date(2026, 8, 31, 6, 59, 0, 0, time.UTC)
	cases := []struct {
		name                          string
		mode, delay, expression, zone string
		now                           time.Time
		runs                          int
		want                          time.Time
	}{
		{"immediate первый", "immediate", "", "", "", moscowMorning, 0, moscowMorning},
		{"immediate следующий", "immediate", "", "", "", moscowMorning, 3, moscowMorning},
		{"after первый", "after", "1h", "", "", moscowMorning, 0, moscowMorning},
		{"after от завершения", "after", "1h", "", "", moscowMorning, 1, moscowMorning.Add(time.Hour)},
		{"cron ближайший", "cron", "", "0 10 * * *", "Europe/Moscow", moscowMorning, 0, time.Date(2026, 8, 31, 7, 0, 0, 0, time.UTC)},
		{"cron пропускает прошедший", "cron", "", "0 10 * * *", "Europe/Moscow", moscowMorning.Add(2 * time.Hour), 1, time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, schedule, err := ParseConfig(tc.mode, tc.delay, tc.expression, tc.zone, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := schedule.Next(tc.now, tc.runs); !got.Equal(tc.want) {
				t.Fatalf("следующая точка %s, ожидалась %s", got, tc.want)
			}
		})
	}
}

// TestParseConfigRejectsAmbiguity защищает CLI от молчаливого выбора режима.
func TestParseConfigRejectsAmbiguity(t *testing.T) {
	for _, input := range [][5]string{
		{"immediate", "1h", "", "", ""},
		{"after", "", "", "", ""},
		{"cron", "", "0 10 * * *", "", ""},
		{"cron", "", "61 * * * *", "Europe/Moscow", ""},
		{"unknown", "", "", "", ""},
		{"immediate", "", "", "", "0"},
	} {
		if _, _, err := ParseConfig(input[0], input[1], input[2], input[3], input[4]); err == nil {
			t.Fatalf("неверная конфигурация принята: %q", input)
		}
	}
}

// TestSeriesLifecycleAndStopBarrier проверяет прогресс, лимитируемые счётчики и
// главный инвариант: второй run нельзя создать, пока текущий не завершён.
func TestSeriesLifecycleAndStopBarrier(t *testing.T) {
	root := t.TempDir()
	owner, err := Create(root, Config{Mode: Immediate, MaxRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	next := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	if err = owner.SetNext(next); err != nil {
		t.Fatal(err)
	}
	created := 0
	started, err := owner.StartRun(func() (string, error) {
		created++
		return "run-one", nil
	}, ignoreRollback)
	if err != nil || !started {
		t.Fatalf("первый run не начат: %t, %v", started, err)
	}
	if _, err = owner.StartRun(func() (string, error) {
		created++
		return "run-two", nil
	}, ignoreRollback); err == nil {
		t.Fatal("параллельный run не был запрещён")
	}
	if created != 1 {
		t.Fatalf("callback создания вызван %d раз", created)
	}
	if err = owner.FinishRun(nil); err != nil {
		t.Fatal(err)
	}
	seriesID := owner.Snapshot().SeriesID
	if err = RequestStop(root, seriesID); err != nil {
		t.Fatal(err)
	}
	started, err = owner.StartRun(func() (string, error) {
		created++
		return "forbidden", nil
	}, ignoreRollback)
	if err != nil || started || created != 1 {
		t.Fatalf("stop пропустил новый run: started=%t created=%d err=%v", started, created, err)
	}
	if err = owner.FinishSeries(Stopped); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(root, seriesID)
	if err != nil || snapshot.State != Stopped || snapshot.RunsStarted != 1 || snapshot.RunsFinished != 1 || !snapshot.StopRequested {
		t.Fatalf("неверный финальный снимок: %+v, %v", snapshot, err)
	}
}

// TestAppSeriesTemplateSurvivesControllerRestart фиксирует главное отличие
// app-native серии от legacy: завершение создавшего её turn освобождает lock, а
// следующий heartbeat восстанавливает неизменяемый вход и продолжает тот же ID.
func TestAppSeriesTemplateSurvivesControllerRestart(t *testing.T) {
	root := t.TempDir()
	want := AppTemplate{
		WorkflowJSON: `{"id":"repeat","steps":[{"id":"work","type":"agent","prompt":"work","dependsOn":[]}]}`,
		Task:         "постановка", Comment: "комментарий", CWD: t.TempDir(),
		InitiatorThreadID: "controller", ParentRunID: "parent",
	}
	owner, err := CreateApp(root, Config{Mode: After, Delay: "10m"}, want)
	if err != nil {
		t.Fatal(err)
	}
	seriesID := owner.Snapshot().SeriesID
	if _, err = Open(root, seriesID); !errors.Is(err, ErrSeriesLocked) {
		t.Fatalf("второй контроллер не увидел занятый lock: %v", err)
	}
	if err = owner.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAppTemplate(root, seriesID)
	if err != nil || got != want {
		t.Fatalf("шаблон после перезапуска = %+v, %v", got, err)
	}
	reopened, err := Open(root, seriesID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot := reopened.Snapshot()
	if snapshot.Version != 2 || snapshot.Driver != AppDriver || snapshot.State != Waiting {
		t.Fatalf("неверно восстановлена app-серия: %+v", snapshot)
	}
}

// TestSeriesParentSync проверяет протокол N2 на настоящих каталогах. Прежде чем
// Create вернёт владельца, каждое имя от root до series/<id> должно быть сохранено
// в родителе; относительный root обязан давать тот же абсолютный порядок.
func TestSeriesParentSync(t *testing.T) {
	for _, tc := range []struct {
		name     string
		depth    int
		relative bool
	}{
		{"существующий root", 0, false},
		{"новый относительный root", 1, true},
		{"вложенный root", 3, false},
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
			seriesBase := filepath.Join(root, "series")
			want = append(want, root, seriesBase)
			var synced []string
			owner, err := create(inputRoot, Config{Mode: Immediate}, func(path string) error {
				synced = append(synced, path)
				return syncDirectory(path)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if !slices.Equal(synced, want) {
				t.Fatalf("порядок Sync: %v; нужен: %v", synced, want)
			}
			if _, err = Load(root, owner.Snapshot().SeriesID); err != nil {
				t.Fatalf("сохранённая серия не читается: %v", err)
			}
		})
	}
}

// TestSeriesParentSyncFailure не разрешает успешно открыть серию, если имя root
// или series/<id> не удалось закрепить в родителе. Повтор снова делает Sync даже
// для оставшихся общих папок, а частный каталог не остаётся после ошибки.
func TestSeriesParentSyncFailure(t *testing.T) {
	for _, failAt := range []string{"root", "series"} {
		t.Run(failAt, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "nested", "runs")
			seriesBase := filepath.Join(root, "series")
			failedDir := base
			if failAt == "series" {
				failedDir = seriesBase
			}
			failure := errors.New("отказ Sync родителя")
			attempts := 0
			syncParent := func(path string) error {
				if path == failedDir {
					attempts++
					return failure
				}
				return syncDirectory(path)
			}
			for range 2 {
				owner, err := create(root, Config{Mode: Immediate}, syncParent)
				if owner != nil || !errors.Is(err, failure) {
					if owner != nil {
						_ = owner.Close()
					}
					t.Fatalf("Create принял несохранённый каталог: owner=%v err=%v", owner != nil, err)
				}
				entries, readErr := os.ReadDir(seriesBase)
				if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
					t.Fatal(readErr)
				}
				if len(entries) != 0 {
					t.Fatalf("после отказа остался частный каталог серии: %v", entries)
				}
			}
			if attempts != 2 {
				t.Fatalf("повтор обошёл несостоявшийся Sync: попыток %d", attempts)
			}
		})
	}
}

// TestStartRunRollsBackUnpublishedRun проверяет откат и обе причины двойного сбоя.
func TestStartRunRollsBackUnpublishedRun(t *testing.T) {
	owner, err := Create(t.TempDir(), Config{Mode: Immediate})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	saveFailure, cleanupFailure := errors.New("series.json недоступен"), errors.New("run не удалён")
	cleanupCalled := false
	started, err := owner.startRun(func() (string, error) { return "run-orphan", nil }, func(string) error {
		cleanupCalled = true
		return cleanupFailure
	}, func() (bool, error) { return false, saveFailure })
	if started || !cleanupCalled || !errors.Is(err, saveFailure) || !errors.Is(err, cleanupFailure) {
		t.Fatalf("неверный результат отката: started=%t cleanup=%t err=%v", started, cleanupCalled, err)
	}
	snapshot := owner.Snapshot()
	if snapshot.RunsStarted != 0 || snapshot.RunsFinished != 0 || snapshot.CurrentRunID != "" || snapshot.State != Waiting {
		t.Fatalf("не восстановлено состояние до создания run: %+v", snapshot)
	}
}

// TestStopIsSerializedWithLaunch воспроизводит настоящую межпроцессную границу
// через две горутины: stop не может вклиниться внутрь уже начатого StartRun.
func TestStopIsSerializedWithLaunch(t *testing.T) {
	root := t.TempDir()
	owner, err := Create(root, Config{Mode: Immediate})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	seriesID := owner.Snapshot().SeriesID
	entered, release := make(chan struct{}), make(chan struct{})
	launchDone := make(chan error, 1)
	go func() {
		_, startErr := owner.StartRun(func() (string, error) {
			close(entered)
			<-release
			return "run-linearized-before-stop", nil
		}, ignoreRollback)
		launchDone <- startErr
	}()
	<-entered
	if err = requestStop(root, seriesID, true); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("stop не обнаружил занятый launch.lock: %v", err)
	}
	if snapshot, loadErr := Load(root, seriesID); loadErr != nil || snapshot.StopRequested {
		t.Fatalf("stop-маркер опубликован до захвата барьера: %+v, %v", snapshot, loadErr)
	}
	close(release)
	if err = <-launchDone; err != nil {
		t.Fatal(err)
	}
	if err = RequestStop(root, seriesID); err != nil {
		t.Fatal(err)
	}
	started, err := owner.StartRun(func() (string, error) {
		return "", errors.New("callback не должен вызываться")
	}, ignoreRollback)
	if err != nil || started {
		t.Fatalf("после линейризованного stop разрешён следующий run: %t, %v", started, err)
	}
}
