package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/teamreport"
)

// metricsCommand только читает team.json. --at задаёт конец измерения ожиданий;
// без него используется последний сохранённый факт, поэтому вывод воспроизводим.
func metricsCommand(args []string, out io.Writer, deps dependencies) error {
	positionals, values, err := parseOptions(args, map[string]bool{"root": true, "at": true})
	if err != nil {
		return err
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return fmt.Errorf("использование: lawa metrics <run-id> [--root <путь>] [--at <RFC3339>]")
	}
	var at time.Time
	if value, ok := values["at"]; ok {
		at, err = time.Parse(time.RFC3339Nano, value)
		if err != nil || at.IsZero() {
			return fmt.Errorf("--at требует ненулевую дату RFC3339")
		}
	}
	root, err := resolveRoot(values["root"], deps.userHomeDir)
	if err != nil {
		return err
	}
	chat, err := runstore.ReadTeam(root, positionals[0])
	if err != nil {
		return err
	}
	if chat.Room == nil {
		return fmt.Errorf("метрики доступны для командного заказа офиса")
	}
	data, err := json.Marshal(teamreport.Build(chat, at))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}
