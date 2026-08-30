package buildinfo

import "testing"

// TestCodexVersion фиксирует преобразование release-тега и локальной dev-сборки
// в clientInfo. Оно защищает единый источник версии от повторного хардкода.
func TestCodexVersion(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })
	for _, tc := range []struct{ version, want string }{
		{"v0.1.0", "0.1.0"},
		{"1.2.3", "1.2.3"},
		{"dev", "dev"},
	} {
		Version = tc.version
		if got := CodexVersion(); got != tc.want {
			t.Fatalf("CodexVersion() для %q = %q, нужно %q", tc.version, got, tc.want)
		}
	}
}
