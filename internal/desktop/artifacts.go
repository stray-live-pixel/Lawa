package desktop

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/stray-live-pixel/Lawa/assets"
)

// shellQuote используется только в создаваемом launcher и административном
// запросе AppleScript. Даже апострофы и пробелы в install-dir остаются данными.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// xmlText экранирует путь в текстовом узле plist, включая символы & и <.
func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// writeFile атомарно заменяет один служебный файл. Сбой записи не оставляет
// обрезанный ключ, plist или shell-script; temp располагается на том же томе.
func writeFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".lawa-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

// writeBundle создаёт служебный .app, а не вторую копию программы: shell
// выполняет установленный бинарник. Установка из архива не требует Node, Xcode,
// iconutil или скачивания иконки. LaunchServices видит обычный значок Applications.
func writeBundle(app, executable string) error {
	// Не перезаписываем одноимённое чужое приложение или ссылку на него.
	if info, err := os.Lstat(app); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("путь приложения занят: %s", app)
		}
		contents, readErr := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
		if readErr != nil || !bytes.Contains(contents, []byte("<string>app.lawa.launcher</string>")) {
			return fmt.Errorf("%s не является launcher Lawa; выберите для него другое имя", app)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	for _, dir := range []string{filepath.Join(app, "Contents"), filepath.Join(app, "Contents", "MacOS"), filepath.Join(app, "Contents", "Resources")} {
		if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
			return fmt.Errorf("каталог приложения занят: %s", dir)
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	info := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>app.lawa.launcher</string>
<key>CFBundleName</key><string>Lawa</string>
<key>CFBundleDisplayName</key><string>Lawa</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleExecutable</key><string>Lawa</string>
<key>CFBundleIconFile</key><string>Lawa.icns</string>
<key>CFBundleVersion</key><string>1</string>
<key>LSUIElement</key><true/>
</dict></plist>`
	launcher := "#!/bin/sh\nexec " + shellQuote(executable) + " ui --finder\n"
	icon, err := iconData()
	if err != nil {
		return err
	}
	for _, file := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{filepath.Join(app, "Contents", "Info.plist"), []byte(info), 0644},
		{filepath.Join(app, "Contents", "MacOS", "Lawa"), []byte(launcher), 0755},
		{filepath.Join(app, "Contents", "Resources", "Lawa.icns"), icon, 0644},
	} {
		if err = writeFile(file.path, file.data, file.mode); err != nil {
			return err
		}
	}
	return nil
}

// writeAgent сохраняет пользовательский job отдельно от системного Applications.
// Никакие файлы home не создаются административным процессом.
func writeAgent(p Paths, executable string) error {
	if err := os.MkdirAll(filepath.Dir(p.Agent), 0755); err != nil {
		return err
	}
	agent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>serve</string></array>
<key>RunAtLoad</key><true/>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>`, label, xmlText(executable), xmlText(filepath.Join(p.Dir, "server.log")), xmlText(filepath.Join(p.Dir, "server.log")))
	return writeFile(p.Agent, []byte(agent), 0644)
}

// iconData кодирует встроенный логотип в ICNS (PNG-представление 512×512).
// Размеры записываются big-endian согласно формату контейнера macOS.
func iconData() ([]byte, error) {
	source, err := png.Decode(bytes.NewReader(assets.LawaLogoPNG))
	if err != nil {
		return nil, err
	}
	scaled := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	bounds := source.Bounds()
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			scaled.Set(x, y, source.At(bounds.Min.X+x*bounds.Dx()/512, bounds.Min.Y+y*bounds.Dy()/512))
		}
	}
	var pngBytes bytes.Buffer
	if err = png.Encode(&pngBytes, scaled); err != nil {
		return nil, err
	}
	var result bytes.Buffer
	result.WriteString("icns")
	_ = binary.Write(&result, binary.BigEndian, uint32(pngBytes.Len()+16))
	result.WriteString("ic09")
	_ = binary.Write(&result, binary.BigEndian, uint32(pngBytes.Len()+8))
	result.Write(pngBytes.Bytes())
	return result.Bytes(), nil
}
