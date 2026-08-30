package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseReleaseVersion проверяет строгий стабильный формат и числовое, а не
// лексикографическое сравнение. От этого решения зависит выбор исполняемого asset.
func TestParseReleaseVersion(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  releaseVersion
		ok    bool
	}{
		{"v0.1.0", releaseVersion{0, 1, 0}, true},
		{"v1.10.2", releaseVersion{1, 10, 2}, true},
		{"1.2.3", releaseVersion{}, false},
		{"v1.2", releaseVersion{}, false},
		{"v1.02.3", releaseVersion{}, false},
		{"v1.2.3-rc.1", releaseVersion{}, false},
	} {
		got, err := parseReleaseVersion(tc.value)
		if (err == nil) != tc.ok || tc.ok && got != tc.want {
			t.Errorf("parseReleaseVersion(%q) = %v, %v; нужно %v, ok=%v", tc.value, got, err, tc.want, tc.ok)
		}
	}
	if compareReleaseVersions(releaseVersion{1, 10, 0}, releaseVersion{1, 9, 9}) <= 0 {
		t.Fatal("1.10.0 должна быть новее 1.9.9")
	}
}

// TestUpdateCommandDevBuild гарантирует, что локальная сборка не угадывает
// release и даже не обращается к сети.
func TestUpdateCommandDevBuild(t *testing.T) {
	called := false
	deps := updateDependencies{
		currentVersion: "dev",
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, errors.New("сеть не должна вызываться")
		})},
	}
	err := updateCommand(t.Context(), nil, io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "dev-сборка") || called {
		t.Fatalf("неожиданный результат dev update: called=%v, err=%v", called, err)
	}
}

// TestUpdateCommandAlreadyCurrent не загружает assets и сообщает обе версии.
func TestUpdateCommandAlreadyCurrent(t *testing.T) {
	deps := basicUpdateDependencies(t, &releaseFixture{tag: "v0.1.0"})
	var out bytes.Buffer
	if err := updateCommand(t.Context(), nil, &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "уже актуальна") || !strings.Contains(out.String(), "v0.1.0") {
		t.Fatalf("непонятный ответ: %q", out.String())
	}
}

// TestUpdateCommandDownloadsVerifiedInstaller проходит всю Go-часть обновления.
// Запуск заменён функцией, поэтому загруженный shell не получает полномочий теста.
func TestUpdateCommandDownloadsVerifiedInstaller(t *testing.T) {
	installer := []byte("#!/bin/sh\nprintf 'verified installer\\n'\n")
	sum := sha256.Sum256(installer)
	assets := map[string][]byte{
		"install.sh": installer,
		"SHA256SUMS": []byte(hex.EncodeToString(sum[:]) + "  install.sh\n"),
	}
	deps := basicUpdateDependencies(t, &releaseFixture{tag: "v0.2.0", assets: assets})
	var gotScript []byte
	var gotArgs []string
	deps.runInstaller = func(_ context.Context, path string, args []string, _, _ io.Writer) error {
		var err error
		gotScript, err = os.ReadFile(path)
		gotArgs = append([]string(nil), args...)
		return err
	}
	var out bytes.Buffer
	if err := updateCommand(t.Context(), []string{"--yes", "--install-plantuml", "--codex-home", filepath.Join(t.TempDir(), "codex home")}, &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotScript, installer) {
		t.Fatalf("запущен другой install.sh: %q", gotScript)
	}
	// Каталог executable создаётся helper-ом в отдельном TempDir, поэтому здесь
	// проверяем смысл аргументов без привязки к случайному абсолютному пути.
	if len(gotArgs) != 8 || gotArgs[0] != "--version" || gotArgs[1] != "v0.2.0" || gotArgs[2] != "--install-dir" || gotArgs[4] != "--codex-home" || gotArgs[6] != "--yes" || gotArgs[7] != "--install-plantuml" {
		t.Fatalf("неожиданные аргументы установщика: %#v", gotArgs)
	}
	if !strings.Contains(out.String(), "v0.1.0 → v0.2.0") || !strings.Contains(out.String(), "Скилл:") {
		t.Fatalf("нет понятного плана обновления: %q", out.String())
	}
}

// TestUpdateCommandRejectsChecksum не запускает повреждённый asset.
func TestUpdateCommandRejectsChecksum(t *testing.T) {
	assets := map[string][]byte{
		"install.sh": []byte("changed"),
		"SHA256SUMS": []byte(strings.Repeat("0", 64) + "  install.sh\n"),
	}
	deps := basicUpdateDependencies(t, &releaseFixture{tag: "v0.2.0", assets: assets})
	called := false
	deps.runInstaller = func(context.Context, string, []string, io.Writer, io.Writer) error {
		called = true
		return nil
	}
	err := updateCommand(t.Context(), nil, io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "checksum") || called {
		t.Fatalf("повреждённый скрипт не отклонён: called=%v, err=%v", called, err)
	}
}

// TestUpdateCommandNetworkError сохраняет исходную сетевую причину в диагностике.
func TestUpdateCommandNetworkError(t *testing.T) {
	deps := basicUpdateDependencies(t, nil)
	deps.latestReleaseURL = "https://release.invalid/latest"
	deps.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
	err := updateCommand(t.Context(), nil, io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("потеряна причина сетевой ошибки: %v", err)
	}
}

// TestUpdateCommandRequiresCompleteRelease запрещает запуск при частичной
// публикации, даже если новый тег уже виден через releases/latest.
func TestUpdateCommandRequiresCompleteRelease(t *testing.T) {
	deps := basicUpdateDependencies(t, &releaseFixture{
		tag:    "v0.2.0",
		assets: map[string][]byte{"install.sh": []byte("script")},
	})
	err := updateCommand(t.Context(), nil, io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("частичный release не отклонён: %v", err)
	}
}

// TestProductionUpdateURL фиксирует официальный репозиторий. Ошибка в нём
// выглядела бы для пользователя как отсутствие релизов или загрузка чужого кода.
func TestProductionUpdateURL(t *testing.T) {
	deps := productionUpdateDependencies()
	want := "https://api.github.com/repos/stray-live-pixel/Lawa/releases/latest"
	if deps.latestReleaseURL != want {
		t.Fatalf("latest release URL = %q, нужно %q", deps.latestReleaseURL, want)
	}
}

// TestParseUpdateArguments отделяет обычное подтверждение файлов Lawa от
// повышенного разрешения на системную установку PlantUML.
func TestParseUpdateArguments(t *testing.T) {
	if _, err := parseUpdateArguments([]string{"--install-plantuml"}); err == nil {
		t.Fatal("--install-plantuml без --yes должен быть отклонён")
	}
	parsed, err := parseUpdateArguments([]string{"--yes", "--install-plantuml", "--codex-home=/tmp/codex home"})
	if err != nil || !parsed.yes || !parsed.installPlantUML || parsed.codexHome != "/tmp/codex home" {
		t.Fatalf("неверный разбор: %#v, %v", parsed, err)
	}
}

// roundTripFunc позволяет проверить сетевые отказы без DNS и внешнего сервера.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func basicUpdateDependencies(t *testing.T, fixture *releaseFixture) updateDependencies {
	t.Helper()
	latestURL := ""
	client := &http.Client{}
	if fixture != nil {
		latestURL = "https://release.test/latest"
		client = &http.Client{Transport: fixture}
	}
	executable := filepath.Join(t.TempDir(), "bin", "lawa")
	return updateDependencies{
		currentVersion:   "v0.1.0",
		latestReleaseURL: latestURL,
		httpClient:       client,
		executable:       func() (string, error) { return executable, nil },
		userHomeDir:      func() (string, error) { return t.TempDir(), nil },
		getenv:           func(string) string { return "" },
		runInstaller:     func(context.Context, string, []string, io.Writer, io.Writer) error { return nil },
	}
}

// releaseFixture отвечает как GitHub API и CDN, но остаётся обычным Transport:
// тесты не открывают сетевой порт и одинаково работают в sandbox и CI.
type releaseFixture struct {
	tag    string
	assets map[string][]byte
}

func (fixture *releaseFixture) RoundTrip(request *http.Request) (*http.Response, error) {
	status := http.StatusOK
	var content []byte
	if request.URL.Path == "/latest" {
		var body bytes.Buffer
		fmt.Fprintf(&body, `{"tag_name":%q,"assets":[`, fixture.tag)
		first := true
		for name := range fixture.assets {
			if !first {
				body.WriteByte(',')
			}
			first = false
			fmt.Fprintf(&body, `{"name":%q,"browser_download_url":%q}`, name, "https://release.test/assets/"+name)
		}
		body.WriteString("]}")
		content = body.Bytes()
	} else {
		name := strings.TrimPrefix(request.URL.Path, "/assets/")
		var exists bool
		content, exists = fixture.assets[name]
		if !exists {
			status = http.StatusNotFound
		}
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(content)),
		Request:    request,
	}, nil
}
