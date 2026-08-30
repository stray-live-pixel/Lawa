package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/buildinfo"
)

const (
	releaseRepository = "stray-live-pixel/Lawa"
	maxReleaseJSON    = 2 << 20
	maxChecksumFile   = 1 << 20
	maxInstallerFile  = 2 << 20
)

// updateDependencies отделяет сеть, окружение и запуск shell от решения об
// обновлении. Production использует GitHub и /bin/sh, а тесты могут подтвердить
// URL, checksum и аргументы, не исполняя загруженный файл.
type updateDependencies struct {
	currentVersion   string
	latestReleaseURL string
	httpClient       *http.Client
	executable       func() (string, error)
	userHomeDir      func() (string, error)
	getenv           func(string) string
	runInstaller     func(context.Context, string, []string, io.Writer, io.Writer) error
}

// productionUpdateDependencies задаёт ограниченный по времени HTTP-клиент и
// прямой запуск проверенного скрипта. Shell получает путь и аргументы раздельно:
// пользовательские пути не интерполируются в командную строку.
func productionUpdateDependencies() updateDependencies {
	return updateDependencies{
		currentVersion:   buildinfo.Version,
		latestReleaseURL: "https://api.github.com/repos/" + releaseRepository + "/releases/latest",
		httpClient:       &http.Client{Timeout: 30 * time.Second},
		executable:       os.Executable,
		userHomeDir:      os.UserHomeDir,
		getenv:           os.Getenv,
		runInstaller: func(ctx context.Context, script string, args []string, out, stderr io.Writer) error {
			command := exec.CommandContext(ctx, "/bin/sh", append([]string{script}, args...)...)
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, out, stderr
			if err := command.Run(); err != nil {
				return fmt.Errorf("установщик завершился с ошибкой: %w", err)
			}
			return nil
		},
	}
}

// updateArguments содержит только разрешения и путь скиллов. Каталог бинарника
// всегда выводится из текущего executable: обновление не должно случайно создать
// вторую Lawa в другом месте.
type updateArguments struct {
	yes, installPlantUML bool
	codexHome            string
}

// updateCommand проверяет release до загрузки исполняемого скрипта, затем сверяет
// SHA-256 и передаёт установку versioned install.sh. Сам Go-код не дублирует
// транзакцию замены бинарника, скилла и PATH из установщика.
func updateCommand(ctx context.Context, args []string, out, stderr io.Writer, deps updateDependencies) error {
	parsed, err := parseUpdateArguments(args)
	if err != nil {
		return err
	}
	if deps.currentVersion == "dev" {
		return errors.New("dev-сборка не обновляется автоматически; установите официальный release через install.sh")
	}
	current, err := parseReleaseVersion(deps.currentVersion)
	if err != nil {
		return fmt.Errorf("встроенная версия %q не является release-тегом: %w", deps.currentVersion, err)
	}
	release, err := fetchLatestRelease(ctx, deps)
	if err != nil {
		return err
	}
	latest, err := parseReleaseVersion(release.TagName)
	if err != nil {
		return fmt.Errorf("GitHub вернул неподдерживаемый тег %q: %w", release.TagName, err)
	}
	if compareReleaseVersions(current, latest) >= 0 {
		_, err = fmt.Fprintf(out, "Lawa %s уже актуальна; последняя стабильная версия — %s.\n", deps.currentVersion, release.TagName)
		return err
	}

	executable, err := deps.executable()
	if err != nil {
		return fmt.Errorf("определить путь текущего бинарника: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("нормализовать путь текущего бинарника: %w", err)
	}
	if parsed.codexHome == "" {
		parsed.codexHome = deps.getenv("CODEX_HOME")
	}
	if parsed.codexHome == "" {
		home, homeErr := deps.userHomeDir()
		if homeErr != nil {
			return fmt.Errorf("найти домашнюю папку для скилла: %w", homeErr)
		}
		parsed.codexHome = filepath.Join(home, ".codex")
	}
	parsed.codexHome, err = filepath.Abs(parsed.codexHome)
	if err != nil {
		return fmt.Errorf("нормализовать CODEX_HOME: %w", err)
	}

	assets, err := requiredReleaseAssets(release)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(out,
		"Доступно обновление: %s → %s.\nБинарник: %s\nСкилл: %s\n",
		deps.currentVersion, release.TagName, executable,
		filepath.Join(parsed.codexHome, "skills", "lawa", "SKILL.md")); err != nil {
		return err
	}

	temporary, err := os.MkdirTemp("", "lawa-update-")
	if err != nil {
		return fmt.Errorf("создать временную папку обновления: %w", err)
	}
	defer os.RemoveAll(temporary)
	checksums, err := downloadReleaseAsset(ctx, deps, assets["SHA256SUMS"], maxChecksumFile)
	if err != nil {
		return fmt.Errorf("загрузить SHA256SUMS: %w", err)
	}
	installer, err := downloadReleaseAsset(ctx, deps, assets["install.sh"], maxInstallerFile)
	if err != nil {
		return fmt.Errorf("загрузить install.sh: %w", err)
	}
	wantChecksum, err := checksumFor(checksums, "install.sh")
	if err != nil {
		return err
	}
	gotChecksum := sha256.Sum256(installer)
	if !strings.EqualFold(wantChecksum, hex.EncodeToString(gotChecksum[:])) {
		return errors.New("checksum install.sh не совпал; текущая установка не изменена")
	}
	scriptPath := filepath.Join(temporary, "install.sh")
	if err = os.WriteFile(scriptPath, installer, 0o600); err != nil {
		return fmt.Errorf("сохранить проверенный install.sh: %w", err)
	}

	installerArgs := []string{
		"--version", release.TagName,
		"--install-dir", filepath.Dir(executable),
		"--codex-home", parsed.codexHome,
	}
	if parsed.yes {
		installerArgs = append(installerArgs, "--yes")
	}
	if parsed.installPlantUML {
		installerArgs = append(installerArgs, "--install-plantuml")
	}
	if err = deps.runInstaller(ctx, scriptPath, installerArgs, out, stderr); err != nil {
		return err
	}
	return nil
}

// parseUpdateArguments не принимает произвольные параметры установщика. Так
// новый release не сможет незаметно расширить полномочия старого `lawa update`.
func parseUpdateArguments(args []string) (updateArguments, error) {
	var parsed updateArguments
	for index := 0; index < len(args); index++ {
		switch argument := args[index]; {
		case argument == "--yes":
			if parsed.yes {
				return parsed, errors.New("параметр --yes повторён")
			}
			parsed.yes = true
		case argument == "--install-plantuml":
			if parsed.installPlantUML {
				return parsed, errors.New("параметр --install-plantuml повторён")
			}
			parsed.installPlantUML = true
		case argument == "--codex-home" || strings.HasPrefix(argument, "--codex-home="):
			if parsed.codexHome != "" {
				return parsed, errors.New("параметр --codex-home повторён")
			}
			if argument == "--codex-home" {
				index++
				if index >= len(args) {
					return parsed, errors.New("параметру --codex-home нужно значение")
				}
				parsed.codexHome = args[index]
			} else {
				parsed.codexHome = strings.TrimPrefix(argument, "--codex-home=")
			}
			if strings.TrimSpace(parsed.codexHome) == "" {
				return parsed, errors.New("параметру --codex-home нужно непустое значение")
			}
		default:
			return parsed, fmt.Errorf("неизвестный параметр update %q", argument)
		}
	}
	if parsed.installPlantUML && !parsed.yes {
		return parsed, errors.New("--install-plantuml требует --yes: установка системного пакета должна быть явно разрешена для неинтерактивного запуска")
	}
	return parsed, nil
}

// githubRelease сохраняет только поля, необходимые доверенной цепочке обновления.
// DownloadURL приходит от GitHub API; имя сопоставляется строго, без первого
// похожего asset, чтобы checksum относился к исполняемому install.sh.
type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// fetchLatestRelease использует официальный latest endpoint: GitHub исключает
// draft и prerelease, поэтому клиенту не нужно самостоятельно сортировать теги.
func fetchLatestRelease(ctx context.Context, deps updateDependencies) (githubRelease, error) {
	var release githubRelease
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, deps.latestReleaseURL, nil)
	if err != nil {
		return release, fmt.Errorf("создать запрос GitHub Release: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "lawa/"+deps.currentVersion)
	response, err := deps.httpClient.Do(request)
	if err != nil {
		return release, fmt.Errorf("получить последний GitHub Release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return release, fmt.Errorf("получить последний GitHub Release: HTTP %s", response.Status)
	}
	body, err := readLimited(response.Body, maxReleaseJSON)
	if err != nil {
		return release, fmt.Errorf("прочитать ответ GitHub Release: %w", err)
	}
	if err = json.Unmarshal(body, &release); err != nil {
		return release, fmt.Errorf("разобрать ответ GitHub Release: %w", err)
	}
	if release.TagName == "" {
		return release, errors.New("GitHub Release не содержит tag_name")
	}
	return release, nil
}

// requiredReleaseAssets запрещает переход к загрузке, пока release не содержит
// и исполняемый скрипт, и опубликованный рядом checksum-манифест.
func requiredReleaseAssets(release githubRelease) (map[string]string, error) {
	required := map[string]string{"install.sh": "", "SHA256SUMS": ""}
	for _, asset := range release.Assets {
		if _, wanted := required[asset.Name]; wanted && required[asset.Name] == "" {
			required[asset.Name] = asset.DownloadURL
		}
	}
	for name, downloadURL := range required {
		if downloadURL == "" {
			return nil, fmt.Errorf("GitHub Release %s не содержит обязательный asset %s", release.TagName, name)
		}
	}
	return required, nil
}

// downloadReleaseAsset сохраняет контекст отмены, проверяет HTTP-статус и
// ограничивает размер до передачи содержимого checksum-проверке.
func downloadReleaseAsset(ctx context.Context, deps updateDependencies, downloadURL string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "lawa/"+deps.currentVersion)
	response, err := deps.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s для %s", response.Status, downloadURL)
	}
	return readLimited(response.Body, limit)
}

// readLimited отличает ровно допустимый размер от усечённого ответа. Без
// дополнительного байта слишком большой JSON или скрипт выглядел бы корректно
// прочитанным и мог привести к непонятной ошибке разбора или checksum.
func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("ответ превышает предел %d байт", limit)
	}
	return content, nil
}

// checksumFor принимает стандартные текстовый и binary-маркеры утилит sha256,
// но требует точного имени файла и ровно 32 байта digest.
func checksumFor(content []byte, filename string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != filename {
			continue
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return "", fmt.Errorf("SHA256SUMS содержит неверный checksum для %s", filename)
		}
		return strings.ToLower(fields[0]), nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("прочитать SHA256SUMS: %w", err)
	}
	return "", fmt.Errorf("SHA256SUMS не содержит %s", filename)
}

// releaseVersion хранит три числовые компоненты стабильного semver без суффикса.
type releaseVersion [3]int

// parseReleaseVersion принимает только стабильные теги. GitHub latest уже
// исключает draft и prerelease, а строгий формат не позволяет неявно сравнивать
// несовместимые схемы версий или запускать asset с неожиданным именем.
func parseReleaseVersion(value string) (releaseVersion, error) {
	var version releaseVersion
	if !strings.HasPrefix(value, "v") {
		return version, errors.New("ожидается формат vMAJOR.MINOR.PATCH")
	}
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != len(version) {
		return version, errors.New("ожидается формат vMAJOR.MINOR.PATCH")
	}
	for index, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return version, errors.New("компоненты версии должны быть целыми числами без ведущих нулей")
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return version, errors.New("компоненты версии должны быть неотрицательными целыми числами")
		}
		version[index] = number
	}
	return version, nil
}

// compareReleaseVersions возвращает знак числового сравнения слева направо.
func compareReleaseVersions(left, right releaseVersion) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}
