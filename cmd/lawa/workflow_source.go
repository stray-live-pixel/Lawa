package main

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// loadWorkflowSource читает вход CLI и раскрывает Markdown до preflight и
// создания run/серии. Общий снимок не зависит от будущих правок исходных файлов.
func loadWorkflowSource(path string) ([]byte, workflow.Workflow, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, workflow.Workflow{}, err
	}
	// CLI сохраняет обычную семантику локальных путей, включая абсолютные
	// симлинки. Ограниченные os.Root нужны только дочерним запросам агента.
	data, err := readWorkflowFile(absolute)
	if err != nil {
		return nil, workflow.Workflow{}, err
	}
	return workflow.ResolveSource(data, absolute, readWorkflowFile)
}

// readWorkflowFile исключает зависание на FIFO и чтение устройств как JSON
// либо Markdown. Одинаково используется фиксированным запуском и заказом Боссу.
func readWorkflowFile(name string) ([]byte, error) {
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return readOpenedRegularFile(file)
}
