package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMacOSIntegration проверяет реальные LaunchServices и launchd без hosts,
// Keychain и production job. Включается явно в локальной GUI-сессии macOS;
// временный launcher и job имеют отдельные идентификаторы и удаляются тестом.
func TestMacOSIntegration(t *testing.T) {
	if os.Getenv("LAWA_DESKTOP_INTEGRATION") != "1" {
		t.Skip("нужна локальная GUI-сессия macOS")
	}
	p := UserPaths(t.TempDir())
	p.App = filepath.Join(p.Dir, "Lawa.app")
	_ = os.MkdirAll(p.Dir, 0700)
	marker := filepath.Join(p.Dir, "opened")
	executable := filepath.Join(p.Dir, "fake-lawa")
	script := "#!/bin/sh\nif [ \"$1\" = ui ]; then printf opened > " + shellQuote(marker) + "; else exec /bin/sleep 30; fi\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ensureCertificate(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	cert := filepath.Join(p.Dir, "server.pem")
	// Security.framework проверяет SSL-политику с явно заданным локальным anchor.
	// Системное доверие и Keychain эта проверка не меняет.
	if err := command(context.Background(), "/usr/bin/security", "verify-cert", "-c", cert, "-r", cert, "-p", "ssl", "-n", Host, "-N", "-L"); err != nil {
		t.Fatal(err)
	}
	if err := testArtifacts(p, executable); err != nil {
		t.Fatal(err)
	}
	infoPath := filepath.Join(p.App, "Contents", "Info.plist")
	info, _ := os.ReadFile(infoPath)
	info = []byte(strings.ReplaceAll(string(info), "app.lawa.launcher", "app.lawa.launcher.test"+strconv.Itoa(os.Getpid())))
	if err := os.WriteFile(infoPath, info, 0600); err != nil {
		t.Fatal(err)
	}
	defer command(context.Background(), "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", "-u", p.App)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := command(ctx, "/usr/bin/open", "-W", p.App); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("Finder не выполнил launcher: %v", err)
	}
	unique := label + ".test" + strconv.Itoa(os.Getpid())
	serviceID := "gui/" + strconv.Itoa(os.Getuid()) + "/" + unique
	data, _ := os.ReadFile(p.Agent)
	data = []byte(strings.ReplaceAll(string(data), label, unique))
	if err := os.WriteFile(p.Agent, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := command(ctx, "/bin/launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), p.Agent); err != nil {
		t.Fatal(err)
	}
	defer command(context.Background(), "/bin/launchctl", "bootout", serviceID)
	for i := 0; i < 2; i++ {
		if err := command(ctx, "/bin/launchctl", "kickstart", serviceID); err != nil {
			t.Fatal(err)
		}
	}
}
