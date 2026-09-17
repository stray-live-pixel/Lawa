package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/workflow"
)

// officeInput готовит неизменяемый снимок команды до запуска сервера, без записи
// на диск и обращения к Codex. nil означает обычный serve: не создавать заказ.
// Файл читается один раз; его изменение не меняет работающих личностей.
func officeInput(args serveArguments) (*runstore.Input, error) {
	if args.task == "" && args.taskFile == "" {
		return nil, nil
	}
	goal := args.task
	if args.taskFile != "" {
		var err error
		goal, err = readTextArgument(args.taskFile, "цель команды")
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(goal) == "" || len(goal) > 65536 || !utf8.ValidString(goal) {
		return nil, errors.New("цель команды: непустой UTF-8 текст до 64 КБ")
	}
	characters := workflow.DefaultTeamCharacters()
	if args.teamConfig != "" {
		path, err := filepath.Abs(args.teamConfig)
		if err != nil {
			return nil, err
		}
		data, err := readWorkflowFile(path)
		if err != nil {
			return nil, err
		}
		var config struct {
			Characters map[string]workflow.Character `json:"characters"`
		}
		if err := json.Unmarshal(data, &config, json.RejectUnknownMembers(true)); err != nil {
			return nil, fmt.Errorf("конфиг команды: %w", err)
		}
		if config.Characters == nil {
			return nil, errors.New("конфиг команды требует объект characters")
		}
		characters = config.Characters
		if _, exists := characters["boss"]; !exists {
			characters["boss"] = workflow.DefaultTeamCharacters()["boss"]
		}
	}
	if err := workflow.ValidateTeamCharacters(characters); err != nil {
		return nil, err
	}
	definition := workflow.Workflow{ID: "office-boss", Characters: characters, Steps: []workflow.Step{{ID: "boss", Type: "agent", Character: "boss", Prompt: workflow.BossAssignment, DependsOn: []string{}}}}
	data, err := json.Marshal(definition)
	if err != nil {
		return nil, err
	}
	return &runstore.Input{Order: true, Team: true, WorkflowJSON: data, Task: goal, CWD: args.cwd}, nil
}
