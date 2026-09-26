package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/stray-live-pixel/Lawa/internal/reviewruntime"
	"github.com/stray-live-pixel/Lawa/internal/reviewstore"
)

const reviewHelp = `lawa review --prompt <текст> [--cwd <проект>] [--root <хранилище>]
  --prompt-file <файл>  Прочитать постановку из UTF-8 файла вместо --prompt.
  --config <файл>       JSON-переопределения моделей, effort и тарифов этапов.
  --config-json <JSON>  Те же настройки напрямую; несовместимо с --config.
  --no-ui              Выполнить без сервера UI и открытия браузера.
  --codex <путь>        Исполняемый файл Codex app-server.

Без --no-ui открывает /code-review?reviewId=<id>. После завершения UI доступен
до Ctrl+C. Результат и доказательства сохраняются в <root>/<reviewId>.
`

// reviewCommand создаёт неизменяемую конфигурацию и исполняет три этапа.
// UI — наблюдатель: отказ браузера не отменяет review, --no-ui не меняет результат.
func reviewCommand(ctx context.Context, args []string, out, stderr io.Writer, deps dependencies) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(out, reviewHelp)
		return err
	}
	pos, values, err := parseOptionsWithSwitches(args, map[string]bool{
		"prompt": true, "prompt-file": true, "cwd": true, "root": true,
		"config": true, "config-json": true, "codex": true,
	}, map[string]bool{"no-ui": true})
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("review принимает постановку через --prompt или --prompt-file")
	}
	if values["prompt"] != "" && values["prompt-file"] != "" {
		return errors.New("--prompt и --prompt-file взаимоисключающие")
	}
	prompt := values["prompt"]
	if name := values["prompt-file"]; name != "" {
		data, readErr := os.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		prompt = string(data)
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("нужна непустая постановка --prompt или --prompt-file")
	}
	config, err := reviewConfig(values["config"], values["config-json"])
	if err != nil {
		return err
	}
	if executable := values["codex"]; executable != "" {
		if strings.ContainsAny(executable, `/\`) {
			executable, err = filepath.Abs(executable)
			if err != nil {
				return err
			}
		}
		config.CodexExecutable = executable
	}
	root, err := resolveRoot(values["root"], deps.userHomeDir)
	if err != nil {
		return err
	}
	cwd := values["cwd"]
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return err
	}
	store := reviewstore.New(root)
	review, err := store.Create(reviewstore.CreateOptions{Prompt: prompt, CWD: cwd, Config: config})
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(out, "Review:", review.ID); err != nil {
		return err
	}
	_, noUI := values["no-ui"]
	ui := newRunUI(ctx, root, noUI, out, stderr, deps)
	defer ui.Close()
	ui.ShowReview(review.ID)
	engine := reviewruntime.Engine{Store: store, Executable: config.CodexExecutable}
	if deps.reviewRun != nil {
		err = deps.reviewRun(ctx, root, review.ID, config.CodexExecutable)
	} else {
		err = engine.Run(ctx, review.ID)
	}
	if final, loadErr := store.Load(review.ID); loadErr == nil {
		fmt.Fprintf(out, "Состояние: %s. Вердикт: %s. Материалы: %s\n", final.State, final.Verdict, filepath.Join(root, review.ID))
	}
	return ui.Wait(err)
}

// reviewConfig накладывает JSON на встроенные значения до создания review.
// Неизвестные поля запрещены: опечатка не должна незаметно запустить иную модель.
func reviewConfig(file, inline string) (reviewstore.Config, error) {
	cfg := reviewstore.DefaultConfig()
	if file != "" && inline != "" {
		return cfg, errors.New("--config и --config-json взаимоисключающие")
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return cfg, err
		}
		inline = string(data)
	}
	if inline != "" {
		if !strings.HasPrefix(strings.TrimSpace(inline), "{") {
			return cfg, errors.New("конфигурация review должна быть JSON-объектом")
		}
		decoder := json.NewDecoder(strings.NewReader(inline))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return cfg, err
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return cfg, errors.New("ожидался один JSON-объект настроек")
		}
	}
	return cfg, reviewstore.ValidateConfig(cfg)
}

// reviewExecuteCommand — внутренний worker запуска, созданного web UI.
// Отдельный процесс переживает закрытие вкладки; конфигурация читается из snapshot.
func reviewExecuteCommand(ctx context.Context, args []string, stderr io.Writer) error {
	pos, options, err := parseOptionsWithSwitches(args, map[string]bool{"root": true}, map[string]bool{"retry": true})
	if err != nil {
		return err
	}
	if len(pos) != 1 || options["root"] == "" {
		return errors.New("review-execute требует ID и --root")
	}
	engine := reviewruntime.Engine{Store: reviewstore.New(options["root"])}
	if _, retry := options["retry"]; retry {
		return engine.Retry(ctx, pos[0])
	}
	return engine.Run(ctx, pos[0])
}
