package codex

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const inheritedDirectoryPath = "."
const directoryHelperArgument = "__lawa_exec_in_open_directory__"

// Directory удерживает уже проверенный каталог открытым между preflight и всеми
// App Server-сессиями одного run. Строковое имя каталога за это время может быть
// переименовано или заменено симлинком, но файловый дескриптор продолжает указывать
// на исходный объект. Поэтому каталог является capability, а не повторно
// разрешаемым обещанием о том, что по пути всё ещё лежит прежний объект.
//
// Directory допускает одновременный запуск нескольких сессий. Close можно
// вызывать только после их завершения; владельцем жизненного цикла является CLI.
type Directory struct {
	path string
	file *os.File
}

// OpenDirectory открывает существующий абсолютный каталог и связывает его
// канонический путь с тем же объектом файловой системы. Проверка os.SameFile
// закрывает гонку между Open и EvalSymlinks: если путь успели подменить, вызов
// отклоняется, а вызывающий может безопасно повторить всю операцию заново.
func OpenDirectory(path string) (_ *Directory, err error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("cwd должен быть абсолютным путём")
	}
	path = filepath.Clean(path)
	// O_NONBLOCK не меняет работу каталога, но не даёт FIFO или устройству
	// зависнуть до проверки IsDir, если путь подменили объектом другого типа.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("открыть cwd: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, file.Close())
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("проверить открытый cwd: %w", err)
	}
	if !opened.IsDir() {
		return nil, errors.New("cwd должен быть папкой")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("определить канонический cwd: %w", err)
	}
	current, err := os.Stat(canonical)
	if err != nil {
		return nil, fmt.Errorf("повторно проверить канонический cwd: %w", err)
	}
	if !os.SameFile(opened, current) {
		return nil, errors.New("cwd изменился во время проверки; повторите запуск")
	}
	return &Directory{path: filepath.Clean(canonical), file: file}, nil
}

// Path возвращает каноническое пользовательское имя для runstore и диагностики.
// Запуск процесса не использует эту строку и поэтому не зависит от её подмены.
func (d *Directory) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// Close освобождает capability после завершения всех процессов одного run.
func (d *Directory) Close() error {
	if d == nil || d.file == nil {
		return nil
	}
	err := d.file.Close()
	d.file = nil
	return err
}

// bindProcess передаёт каталог первым ExtraFile, то есть fd 3 helper-процесса.
// Helper делает fchdir, после чего относительный RPC cwd "." разрешается от уже
// открытого текущего каталога, а не от изменяемого имени пути.
func (d *Directory) bindProcess(process *exec.Cmd) error {
	if err := d.validate(); err != nil {
		return err
	}
	if len(process.ExtraFiles) != 0 {
		return errors.New("безопасный cwd требует свободный fd 3")
	}
	process.ExtraFiles = []*os.File{d.file}
	return nil
}

// validate проверяет, что capability ещё открыта и по-прежнему является каталогом.
func (d *Directory) validate() error {
	if d == nil || d.file == nil {
		return errors.New("проверенный cwd уже закрыт")
	}
	info, err := d.file.Stat()
	if err != nil {
		return fmt.Errorf("проверить дескриптор cwd: %w", err)
	}
	if !info.IsDir() {
		return errors.New("дескриптор cwd не является папкой")
	}
	return nil
}

// matches подтверждает, что внешний путь относится к удерживаемому объекту.
// Точный inheritedDirectoryPath — адрес текущего каталога внутри App Server;
// прочие ответы сервера сравниваются по идентичности объектов, а не по написанию.
func (d *Directory) matches(path string) (bool, error) {
	if d == nil || d.file == nil {
		return false, errors.New("проверенный cwd уже закрыт")
	}
	if filepath.Clean(path) == inheritedDirectoryPath {
		return true, nil
	}
	want, err := d.file.Stat()
	if err != nil {
		return false, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer file.Close()
	got, err := file.Stat()
	return err == nil && os.SameFile(want, got), err
}

// RunDirectoryHelper обрабатывает только внутренний re-exec текущего бинарника.
// Первый процесс нельзя сразу запустить с Dir=/dev/fd/3: os/exec выполняет chdir
// до переноса ExtraFiles на окончательные номера. Короткий helper наследует fd 3,
// делает fchdir к уже открытому объекту и заменяет себя целевым App Server через
// exec без shell и без нового процесса. Он не получает дополнительных прав:
// executable и argv совпадают с обычным запуском Codex, а единственная capability
// — переданный родителем каталог.
//
// Функцию нужно вызвать в самом начале main/TestMain. При обычном запуске она
// возвращает false; внутренний режим завершается syscall.Exec либо кодом 126.
func RunDirectoryHelper() bool {
	if len(os.Args) < 3 || os.Args[1] != directoryHelperArgument {
		return false
	}
	if err := syscall.Fchdir(3); err != nil {
		fmt.Fprintf(os.Stderr, "lawa: перейти в проверенный cwd: %v\n", err)
		os.Exit(126)
	}
	// После fchdir ядро удерживает cwd как самостоятельную ссылку на объект.
	// Сам дескриптор больше не нужен целевому процессу и не расширяет его права.
	_ = syscall.Close(3)
	if err := syscall.Exec(os.Args[2], append([]string{os.Args[2]}, os.Args[3:]...), os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "lawa: запустить App Server в проверенном cwd: %v\n", err)
		os.Exit(126)
	}
	return true
}
