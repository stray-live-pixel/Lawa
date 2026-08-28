// Команда lawa предоставляет офлайн-проверку workflow. Исполнение через Codex
// добавляется отдельно: успешная проверка схемы не означает успешный запуск задач.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/stray-live-pixel/flows-2/internal/workflow"
)

const help = `Lawa — проверка JSON-workflow для задач AI-агентов.

Команды:
  lawa validate <workflow.json>  Проверить поля, ссылки и отсутствие циклов.
  lawa help                     Показать справку (также -h и --help).

Проверка не запускает агентов, не создаёт файлы и не требует подключения к Codex.
Коды выхода: 0 — проверка/справка успешна; 2 — ошибка ввода или вывода.
Команды run, resume и skill пока не реализованы.
`

// main отделяет код завершения процесса от проверяемой без subprocess логики CLI.
func main() {
	if err := execute(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "lawa:", err)
		os.Exit(2)
	}
}

// execute читает только выбранный файл и пишет результат в out. Ошибки передаются
// вызывающему коду; пустой список аргументов показывает справку без чтения файлов.
func execute(args []string, out io.Writer) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help")) {
		_, err := io.WriteString(out, help)
		return err
	}
	if args[0] != "validate" {
		return fmt.Errorf("неизвестная команда %q; см. lawa help", args[0])
	}
	if len(args) != 2 {
		return fmt.Errorf("использование: lawa validate <workflow.json>")
	}
	f, err := os.Open(args[1])
	if err != nil {
		return fmt.Errorf("открыть workflow: %w", err)
	}
	defer f.Close()
	w, err := workflow.Decode(f)
	if err != nil {
		return fmt.Errorf("проверить %q: %w", args[1], err)
	}
	// %q экранирует управляющие символы в пользовательском идентификаторе.
	_, err = fmt.Fprintf(out, "Workflow %q корректен; шагов: %d.\n", w.ID, len(w.Steps))
	return err
}
