package lawa_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installerFixture создаёт полностью локальный fake release и ограниченный PATH.
// Реальный install.sh по-прежнему выполняется системным /bin/sh; заменяются только
// сеть, платформа, PlantUML и пакетный менеджер, которые тест не вправе трогать.
type installerFixture struct {
	t           *testing.T
	root        string
	home        string
	tools       string
	release     string
	marker      string
	managerLog  string
	downloadLog string
	version     string
	script      string
	shell       string
	installDir  string
	codexHome   string
	withManager bool
	extraEnv    []string
}

func newInstallerFixture(t *testing.T, withManager bool) *installerFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &installerFixture{
		t: t, root: root, version: "v0.1.0", script: "install.sh", shell: "/bin/sh", withManager: withManager,
		home:        filepath.Join(root, "home with spaces"),
		tools:       filepath.Join(root, "tools"),
		release:     filepath.Join(root, "release"),
		marker:      filepath.Join(root, "plantuml-ready"),
		managerLog:  filepath.Join(root, "manager.log"),
		downloadLog: filepath.Join(root, "download.log"),
	}
	fixture.installDir = filepath.Join(fixture.home, ".local", "bin")
	fixture.codexHome = filepath.Join(fixture.home, ".codex")
	for _, directory := range []string{fixture.home, fixture.tools, fixture.release} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fixture.prepareTools()
	fixture.writeRelease(fixture.version, false)
	return fixture
}

func (fixture *installerFixture) prepareTools() {
	fixture.t.Helper()
	// Эти команды не подменяются по смыслу; ссылки лишь не дают тесту случайно
	// увидеть настоящий plantuml, brew или пакетный менеджер из PATH машины.
	for _, name := range []string{"awk", "chmod", "cp", "dirname", "grep", "mkdir", "mktemp", "mv", "readlink", "rm", "sed", "tar"} {
		target, err := exec.LookPath(name)
		if err != nil {
			fixture.t.Fatalf("для теста установщика нужна команда %s: %v", name, err)
		}
		if err = os.Symlink(target, filepath.Join(fixture.tools, name)); err != nil {
			fixture.t.Fatal(err)
		}
	}
	fixture.writeTool("uname", `
case "$1" in
  -s) printf '%s\n' "${FAKE_UNAME_S:-Linux}" ;;
  -m) printf '%s\n' "${FAKE_UNAME_M:-x86_64}" ;;
  *) exit 2 ;;
esac
`)
	fixture.writeTool("id", `printf '%s\n' "${FAKE_UID:-1000}"`)
	fixture.writeTool("curl", `
destination=''
url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) destination=$2; shift 2 ;;
    http://*|https://*) url=$1; shift ;;
    *) shift ;;
  esac
done
[ -n "$destination" ] && [ -n "$url" ] || exit 2
printf '%s\n' "$url" >> "$FAKE_DOWNLOAD_LOG"
source_file=$FAKE_RELEASE_DIR/${url##*/}
[ -f "$source_file" ] || exit 22
cp "$source_file" "$destination"
`)
	fixture.writeTool("plantuml", `
[ -f "$FAKE_PLANTUML_MARKER" ] || exit 1
printf 'PlantUML fake 1.0\n'
`)
	checksum, err := exec.LookPath("sha256sum")
	if err == nil {
		fixture.writeTool("sha256sum", fmt.Sprintf("exec %q \"$@\"", checksum))
	} else {
		shasum, lookupErr := exec.LookPath("shasum")
		if lookupErr != nil {
			fixture.t.Fatalf("для теста нужен sha256sum или shasum: %v", lookupErr)
		}
		fixture.writeTool("sha256sum", fmt.Sprintf("exec %q -a 256 \"$@\"", shasum))
	}
	if fixture.withManager {
		fixture.writeTool("sudo", `
[ "${FAKE_SUDO_FAIL:-0}" = 0 ] || exit 77
exec "$@"
`)
		fixture.writeTool("apt-get", `
printf '%s\n' "$*" >> "$FAKE_MANAGER_LOG"
case "$*" in
  *install*)
    [ "${FAKE_APT_FAIL:-0}" = 0 ] || exit 78
    : > "$FAKE_PLANTUML_MARKER"
    ;;
esac
`)
	}
}

func (fixture *installerFixture) writeTool(name, body string) {
	fixture.t.Helper()
	content := "#!/bin/sh\n" + strings.TrimSpace(body) + "\n"
	if err := os.WriteFile(filepath.Join(fixture.tools, name), []byte(content), 0o755); err != nil {
		fixture.t.Fatal(err)
	}
}

// writeRelease создаёт тот же внешний контракт, что release workflow: архив с
// одним бинарником lawa и SHA256SUMS. Сам fake-бинарник поддерживает version и
// skill, поэтому установщик проверяет согласованность двух компонентов.
func (fixture *installerFixture) writeRelease(version string, corruptChecksum bool) {
	fixture.t.Helper()
	archiveName := "lawa_" + version + "_linux_amd64.tar.gz"
	archivePath := filepath.Join(fixture.release, archiveName)
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	binary := []byte(fmt.Sprintf(`#!/bin/sh
case "$1" in
  version) printf '%%s\n' %q ;;
  skill) printf '# Fake Lawa skill for %%s\n' %q ;;
  *) exit 2 ;;
esac
`, version, version))
	if err := tarWriter.WriteHeader(&tar.Header{Name: "lawa", Mode: 0o755, Size: int64(len(binary))}); err != nil {
		fixture.t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		fixture.t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		fixture.t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		fixture.t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, archive.Bytes(), 0o644); err != nil {
		fixture.t.Fatal(err)
	}
	digest := sha256.Sum256(archive.Bytes())
	checksum := hex.EncodeToString(digest[:])
	if corruptChecksum {
		checksum = strings.Repeat("0", sha256.Size*2)
	}
	line := checksum + "  " + archiveName + "\n"
	if err := os.WriteFile(filepath.Join(fixture.release, "SHA256SUMS"), []byte(line), 0o644); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *installerFixture) run(args ...string) (string, string, error) {
	fixture.t.Helper()
	command := exec.Command(fixture.shell, append([]string{fixture.script}, args...)...)
	command.Dir = "."
	command.Env = append(environmentWithout(os.Environ(),
		"HOME", "SHELL", "PATH", "TMPDIR", "FAKE_RELEASE_DIR", "FAKE_PLANTUML_MARKER",
		"FAKE_MANAGER_LOG", "FAKE_DOWNLOAD_LOG", "FAKE_UNAME_S", "FAKE_UNAME_M",
		"FAKE_UID", "FAKE_SUDO_FAIL", "FAKE_APT_FAIL", "LAWA_TESTING", "LAWA_TEST_TTY"),
		"HOME="+fixture.home,
		"SHELL=/bin/zsh",
		"PATH="+fixture.tools,
		"TMPDIR="+fixture.root,
		"FAKE_RELEASE_DIR="+fixture.release,
		"FAKE_PLANTUML_MARKER="+fixture.marker,
		"FAKE_MANAGER_LOG="+fixture.managerLog,
		"FAKE_DOWNLOAD_LOG="+fixture.downloadLog,
	)
	command.Env = append(command.Env, fixture.extraEnv...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func environmentWithout(environment []string, names ...string) []string {
	blocked := make(map[string]bool, len(names))
	for _, name := range names {
		blocked[name] = true
	}
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if !found || !blocked[name] {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func (fixture *installerFixture) standardArgs(extra ...string) []string {
	base := []string{
		"--version", fixture.version,
		"--install-dir", fixture.installDir,
		"--codex-home", fixture.codexHome,
	}
	return append(base, extra...)
}

func (fixture *installerFixture) installedVersion() string {
	fixture.t.Helper()
	command := exec.Command(filepath.Join(fixture.installDir, "lawa"), "version")
	output, err := command.Output()
	if err != nil {
		fixture.t.Fatalf("проверить установленную версию: %v", err)
	}
	return strings.TrimSpace(string(output))
}

// TestInstallerPlanIsReadOnly фиксирует агентский первый шаг: полный план виден,
// но release assets, профиль, бинарник и скилл ещё не затрагиваются.
func TestInstallerPlanIsReadOnly(t *testing.T) {
	fixture := newInstallerFixture(t, true)
	stdout, stderr, err := fixture.run(fixture.standardArgs("--plan")...)
	if err != nil {
		t.Fatalf("plan: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	for _, expected := range []string{fixture.version, fixture.installDir, filepath.Join(fixture.codexHome, "skills", "lawa", "SKILL.md"), "apt-get install", "Режим --plan"} {
		if !strings.Contains(stdout+stderr, expected) {
			t.Errorf("plan не содержит %q:\n%s\n%s", expected, stdout, stderr)
		}
	}
	for _, path := range []string{fixture.downloadLog, fixture.managerLog, filepath.Join(fixture.installDir, "lawa"), filepath.Join(fixture.home, ".zshrc")} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("--plan изменил %s: %v", path, statErr)
		}
	}
}

// TestInstallerUsesEmbeddedReleaseVersion проверяет one-line установку latest:
// опубликованный workflow-ом скрипт выбирает архив своего тега без --version.
func TestInstallerUsesEmbeddedReleaseVersion(t *testing.T) {
	fixture := newInstallerFixture(t, false)
	source, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	versioned := bytes.Replace(source, []byte("release_version='dev'"), []byte("release_version='"+fixture.version+"'"), 1)
	fixture.script = filepath.Join(fixture.root, "install.sh")
	if err = os.WriteFile(fixture.script, versioned, 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := fixture.run(
		"--install-dir", fixture.installDir,
		"--codex-home", fixture.codexHome,
		"--yes",
	)
	if err != nil || fixture.installedVersion() != fixture.version {
		t.Fatalf("embedded version: %v\n%s\n%s", err, stdout, stderr)
	}
}

// TestInstallerDashCompatibility запускает полную установку самым строгим
// распространённым /bin/sh Linux, а не только проверяет синтаксис.
func TestInstallerDashCompatibility(t *testing.T) {
	if _, err := os.Stat("/bin/dash"); err != nil {
		t.Skip("dash отсутствует")
	}
	fixture := newInstallerFixture(t, false)
	fixture.shell = "/bin/dash"
	stdout, stderr, err := fixture.run(fixture.standardArgs("--yes")...)
	if err != nil || fixture.installedVersion() != fixture.version {
		t.Fatalf("dash: %v\n%s\n%s", err, stdout, stderr)
	}
}

// TestInstallerYesDoesNotAuthorizePlantUML доказывает главную границу прав:
// неинтерактивное подтверждение Lawa не запускает apt-get или sudo.
func TestInstallerYesDoesNotAuthorizePlantUML(t *testing.T) {
	fixture := newInstallerFixture(t, true)
	stdout, stderr, err := fixture.run(fixture.standardArgs("--yes")...)
	if err != nil {
		t.Fatalf("install: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if got := fixture.installedVersion(); got != fixture.version {
		t.Fatalf("установлена версия %q", got)
	}
	skill, err := os.ReadFile(filepath.Join(fixture.codexHome, "skills", "lawa", "SKILL.md"))
	if err != nil || !strings.Contains(string(skill), fixture.version) {
		t.Fatalf("скилл не соответствует бинарнику: %q, %v", skill, err)
	}
	if _, err = os.Stat(fixture.managerLog); !os.IsNotExist(err) {
		t.Fatalf("обычный --yes вызвал пакетный менеджер: %v", err)
	}
	profile, err := os.ReadFile(filepath.Join(fixture.home, ".zshrc"))
	if err != nil || strings.Count(string(profile), fixture.installDir) != 1 {
		t.Fatalf("PATH записан неверно: %q, %v", profile, err)
	}
	if !strings.Contains(stderr, "PNG-схемы недоступны") {
		t.Fatalf("нет финального предупреждения PlantUML: %q", stderr)
	}

	// Повтор той же версии безопасен и не дублирует shell profile.
	if _, _, err = fixture.run(fixture.standardArgs("--yes")...); err != nil {
		t.Fatal(err)
	}
	profile, _ = os.ReadFile(filepath.Join(fixture.home, ".zshrc"))
	if strings.Count(string(profile), fixture.installDir) != 1 {
		t.Fatalf("повтор продублировал PATH: %q", profile)
	}
}

// TestInstallerExplicitPlantUMLPermission проверяет отдельное согласие, sudo,
// пакетный менеджер и обязательный повторный plantuml -version.
func TestInstallerExplicitPlantUMLPermission(t *testing.T) {
	fixture := newInstallerFixture(t, true)
	stdout, stderr, err := fixture.run(fixture.standardArgs("--yes", "--install-plantuml")...)
	if err != nil {
		t.Fatalf("install PlantUML: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	managerCalls, err := os.ReadFile(fixture.managerLog)
	if err != nil || !strings.Contains(string(managerCalls), "update") || !strings.Contains(string(managerCalls), "install -y plantuml") {
		t.Fatalf("не выполнены ожидаемые apt-вызовы: %q, %v", managerCalls, err)
	}
	if !strings.Contains(stdout, "PlantUML проверен: PlantUML fake 1.0") || fixture.installedVersion() != fixture.version {
		t.Fatalf("нет повторной проверки или установки:\n%s", stdout)
	}
}

// TestInstallerWorkingPlantUML не предлагает пакетный менеджер, когда реальная
// проверка команды уже успешна.
func TestInstallerWorkingPlantUML(t *testing.T) {
	fixture := newInstallerFixture(t, true)
	if err := os.WriteFile(fixture.marker, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := fixture.run(fixture.standardArgs("--yes")...)
	if err != nil {
		t.Fatalf("готовый PlantUML: %v\n%s\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "PlantUML fake 1.0") || strings.Contains(stderr, "PNG-схемы недоступны") {
		t.Fatalf("рабочий PlantUML распознан неверно:\n%s\n%s", stdout, stderr)
	}
	if _, statErr := os.Stat(fixture.managerLog); !os.IsNotExist(statErr) {
		t.Fatalf("рабочий PlantUML вызвал пакетный менеджер: %v", statErr)
	}
}

// TestInstallerInteractiveChoices использует тестовый tty, но проходит реальные
// две независимые развилки install.sh: отказ от Lawa и согласие на Lawa без
// предоставления прав пакетному менеджеру.
func TestInstallerInteractiveChoices(t *testing.T) {
	t.Run("отказ от Lawa", func(t *testing.T) {
		fixture := newInstallerFixture(t, true)
		tty := filepath.Join(fixture.root, "answers")
		if err := os.WriteFile(tty, []byte("n\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fixture.extraEnv = []string{"LAWA_TESTING=1", "LAWA_TEST_TTY=" + tty}
		stdout, stderr, err := fixture.run(fixture.standardArgs()...)
		if err != nil || !strings.Contains(stdout, "Установка отменена") {
			t.Fatalf("неожиданный отказ: %v\n%s\n%s", err, stdout, stderr)
		}
		if _, statErr := os.Stat(filepath.Join(fixture.installDir, "lawa")); !os.IsNotExist(statErr) {
			t.Fatalf("отказ оставил бинарник: %v", statErr)
		}
	})

	t.Run("Lawa да, PlantUML нет", func(t *testing.T) {
		fixture := newInstallerFixture(t, true)
		tty := filepath.Join(fixture.root, "answers")
		if err := os.WriteFile(tty, []byte("y\nn\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fixture.extraEnv = []string{"LAWA_TESTING=1", "LAWA_TEST_TTY=" + tty}
		stdout, stderr, err := fixture.run(fixture.standardArgs()...)
		if err != nil {
			t.Fatalf("интерактивная установка: %v\n%s\n%s", err, stdout, stderr)
		}
		if fixture.installedVersion() != fixture.version {
			t.Fatal("Lawa не установлена после отдельного отказа от PlantUML")
		}
		if _, statErr := os.Stat(fixture.managerLog); !os.IsNotExist(statErr) {
			t.Fatalf("отказ от PlantUML вызвал apt: %v", statErr)
		}
	})

	t.Run("два отдельных согласия", func(t *testing.T) {
		fixture := newInstallerFixture(t, true)
		tty := filepath.Join(fixture.root, "answers")
		if err := os.WriteFile(tty, []byte("y\ny\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fixture.extraEnv = []string{"LAWA_TESTING=1", "LAWA_TEST_TTY=" + tty}
		stdout, stderr, err := fixture.run(fixture.standardArgs()...)
		if err != nil {
			t.Fatalf("два согласия: %v\n%s\n%s", err, stdout, stderr)
		}
		if fixture.installedVersion() != fixture.version {
			t.Fatal("Lawa не установлена после двух согласий")
		}
		if _, statErr := os.Stat(fixture.managerLog); statErr != nil {
			t.Fatalf("согласие на PlantUML не вызвало apt: %v", statErr)
		}
	})
}

// TestInstallerPlantUMLFailuresStopBeforeLawa проверяет ошибки повышенного этапа.
// Пакетный менеджер может изменить систему, но пользовательские файлы Lawa ещё
// не заменены и потому не требуют отката.
func TestInstallerPlantUMLFailuresStopBeforeLawa(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withManager bool
		environment string
		want        string
	}{
		{"неизвестный менеджер", false, "", "неизвестен безопасный"},
		{"ошибка sudo", true, "FAKE_SUDO_FAIL=1", "apt-get update"},
		{"ошибка apt", true, "FAKE_APT_FAIL=1", "не установил PlantUML"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newInstallerFixture(t, tc.withManager)
			if tc.environment != "" {
				fixture.extraEnv = []string{tc.environment}
			}
			stdout, stderr, err := fixture.run(fixture.standardArgs("--yes", "--install-plantuml")...)
			if err == nil || !strings.Contains(stderr, tc.want) {
				t.Fatalf("ожидалась ошибка %q: %v\n%s\n%s", tc.want, err, stdout, stderr)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.installDir, "lawa")); !os.IsNotExist(statErr) {
				t.Fatalf("ошибка PlantUML изменила Lawa: %v", statErr)
			}
		})
	}
}

// TestInstallerChecksumFailurePreservesCurrentVersion моделирует неудачное
// обновление после рабочей установки и подтверждает транзакционную границу.
func TestInstallerChecksumFailurePreservesCurrentVersion(t *testing.T) {
	fixture := newInstallerFixture(t, false)
	if stdout, stderr, err := fixture.run(fixture.standardArgs("--yes")...); err != nil {
		t.Fatalf("первая установка: %v\n%s\n%s", err, stdout, stderr)
	}
	fixture.version = "v0.2.0"
	fixture.writeRelease(fixture.version, true)
	stdout, stderr, err := fixture.run(fixture.standardArgs("--yes")...)
	if err == nil || !strings.Contains(stderr, "checksum архива не совпал") {
		t.Fatalf("повреждённый архив принят: %v\n%s\n%s", err, stdout, stderr)
	}
	if got := fixture.installedVersion(); got != "v0.1.0" {
		t.Fatalf("неудачное обновление повредило версию: %s", got)
	}
}

// TestInstallerDownloadFailure не создаёт пользовательские файлы, когда release
// неполон или сеть вернула ошибку до checksum.
func TestInstallerDownloadFailure(t *testing.T) {
	fixture := newInstallerFixture(t, false)
	archive := filepath.Join(fixture.release, "lawa_"+fixture.version+"_linux_amd64.tar.gz")
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := fixture.run(fixture.standardArgs("--yes")...)
	if err == nil || !strings.Contains(stderr, "не удалось скачать") {
		t.Fatalf("ошибка загрузки не объяснена: %v\n%s\n%s", err, stdout, stderr)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.installDir, "lawa")); !os.IsNotExist(statErr) {
		t.Fatalf("ошибка загрузки оставила бинарник: %v", statErr)
	}
}

// TestInstallerWgetFallback подтверждает установку на минимальной системе без curl.
func TestInstallerWgetFallback(t *testing.T) {
	fixture := newInstallerFixture(t, false)
	if err := os.Remove(filepath.Join(fixture.tools, "curl")); err != nil {
		t.Fatal(err)
	}
	fixture.writeTool("wget", `
destination=''
url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -O) destination=$2; shift 2 ;;
    http://*|https://*) url=$1; shift ;;
    *) shift ;;
  esac
done
[ -n "$destination" ] && [ -n "$url" ] || exit 2
source_file=$FAKE_RELEASE_DIR/${url##*/}
[ -f "$source_file" ] || exit 8
cp "$source_file" "$destination"
`)
	stdout, stderr, err := fixture.run(fixture.standardArgs("--yes")...)
	if err != nil || fixture.installedVersion() != fixture.version {
		t.Fatalf("wget fallback: %v\n%s\n%s", err, stdout, stderr)
	}
}

// TestInstallerPreservesSymlinkedProfile поддерживает распространённые dotfiles:
// атомарно меняется конечный файл, а пользовательская ссылка ~/.zshrc остаётся.
func TestInstallerPreservesSymlinkedProfile(t *testing.T) {
	fixture := newInstallerFixture(t, false)
	dotfiles := filepath.Join(fixture.home, "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dotfiles, "zshrc")
	if err := os.WriteFile(target, []byte("# existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("dotfiles", "zshrc"), filepath.Join(fixture.home, ".zshrc")); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := fixture.run(fixture.standardArgs("--yes")...)
	if err != nil {
		t.Fatalf("symlinked profile: %v\n%s\n%s", err, stdout, stderr)
	}
	info, err := os.Lstat(filepath.Join(fixture.home, ".zshrc"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("ссылка profile заменена: %v, %v", info, err)
	}
	content, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(content), fixture.installDir) {
		t.Fatalf("конечный profile не обновлён: %q, %v", content, err)
	}
}

// TestInstallerRejectsUnsupportedPlatform завершается до загрузки release.
func TestInstallerRejectsUnsupportedPlatform(t *testing.T) {
	for _, environment := range []string{"FAKE_UNAME_S=FreeBSD", "FAKE_UNAME_M=riscv64"} {
		t.Run(environment, func(t *testing.T) {
			fixture := newInstallerFixture(t, false)
			fixture.extraEnv = []string{environment}
			stdout, stderr, err := fixture.run(fixture.standardArgs("--yes")...)
			if err == nil || !strings.Contains(stderr, "не поддерживается") {
				t.Fatalf("неподдерживаемая платформа принята: %v\n%s\n%s", err, stdout, stderr)
			}
			if _, statErr := os.Stat(fixture.downloadLog); !os.IsNotExist(statErr) {
				t.Fatalf("до ошибки платформы началась загрузка: %v", statErr)
			}
		})
	}
}
