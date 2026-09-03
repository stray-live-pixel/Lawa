package coordinator

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stray-live-pixel/Lawa/internal/capacity"
	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
	"github.com/stray-live-pixel/Lawa/internal/scheduler"
)

// createAgentPreparationRun создаёт настоящий v4 run: тесты подготовки должны
// проходить через тот же строгий Load и те же memory-файлы, что production resume.
func createAgentPreparationRun(t *testing.T, definition string) (string, runstore.Snapshot, *runstore.LockedRun) {
	t.Helper()
	root := t.TempDir()
	snapshot, err := runstore.CreateAgentGraph(root, runstore.Input{
		WorkflowJSON: []byte(definition), Task: "Сделать MVP\n\nКомментарий: проверить границы", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runstore.OpenLocked(root, snapshot.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return root, snapshot, run
}

// completeAgentVisit переводит visit через обязательные durable границы. Нельзя
// записать Running/Succeeded сразу: v4 требует сначала сохранить chat и turn.
func completeAgentVisit(t *testing.T, run *runstore.LockedRun, visitID, chat string, state scheduler.State, diagnostic string) {
	t.Helper()
	if err := run.UpdateVisit(visitID, scheduler.Unknown, chat, ""); err == nil {
		err = run.SetVisitTurn(visitID, "turn-"+visitID)
		if err == nil {
			err = run.UpdateVisit(visitID, state, chat, diagnostic)
		}
		if err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}
}

// TestPrepareAgentVisitsFIFOAndCapacity проверяет общий порядок двух видов
// работы. Старый Cancelled visit получает единственный slot раньше нового
// Pending, а Pending не становится Starting, пока slot действительно недоступен.
func TestPrepareAgentVisitsFIFOAndCapacity(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"fifo","start":["first","second"],"steps":[
    {"id":"first","type":"agent","prompt":"Первый","after":[]},
    {"id":"second","type":"agent","prompt":"Второй","after":[]}
  ]}`)
	firstID, secondID := initial.Meta.Visits[0].VisitID, initial.Meta.Visits[1].VisitID
	if err := run.ReserveVisits([]string{firstID}); err != nil {
		t.Fatal(err)
	}
	completeAgentVisit(t, run, firstID, "chat-first", scheduler.Cancelled, "остановлено оператором")
	pool, err := capacity.Configure(root, "1")
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := prepareAgentVisits(run, root, pool, true, map[string]bool{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Work) != 1 || prepared.Work[0].VisitID != firstID || prepared.Work[0].kind != agentWorkContinuation || prepared.Work[0].ThreadID != "chat-first" ||
		!reflect.DeepEqual(prepared.WaitingForCapacity, []string{secondID}) {
		t.Fatalf("FIFO либо вид работы потерян: %+v", prepared)
	}
	saved, err := run.Load()
	if err != nil || saved.Meta.Visits[1].State != scheduler.Pending {
		t.Fatalf("ожидающий slot visit был зарезервирован: %+v, %v", saved.Meta.Visits, err)
	}
	if err := releaseAgentWork(prepared.Work); err != nil {
		t.Fatal(err)
	}

	// Execution помечает continuation перед запуском. После освобождения slot
	// следующий FIFO-кандидат резервируется ровно один раз по visitId.
	prepared, err = prepareAgentVisits(run, root, pool, true, map[string]bool{firstID: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = releaseAgentWork(prepared.Work) })
	if len(prepared.Work) != 1 || prepared.Work[0].VisitID != secondID || prepared.Work[0].kind != agentWorkLaunch || len(prepared.WaitingForCapacity) != 0 {
		t.Fatalf("Pending visit не получил освобождённый slot: %+v", prepared)
	}
	saved, err = run.Load()
	if err != nil || saved.Meta.Visits[1].State != scheduler.Starting || saved.Meta.Visits[1].CodexThreadID != "" {
		t.Fatalf("launch не зарезервирован до сети: %+v, %v", saved.Meta.Visits, err)
	}
}

// TestAgentPromptContainsDurableVisitContext фиксирует полезный агенту контекст:
// failed является нормальным after-источником, его техническая диагностика не
// подменяется бизнес-выводом, а несвязанная завершённая ветка остаётся доступна.
func TestAgentPromptContainsDurableVisitContext(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"context","start":["history","source"],"steps":[
    {"id":"history","type":"agent","prompt":"Собери фон","after":[]},
    {"id":"source","type":"agent","prompt":"Запусти тест","after":[]},
    {"id":"checker","type":"agent","prompt":"Разбери результат","after":["source"],"decisions":{
      "stop":{"finish":"failed"},"continue":{"to":["target"]}}},
    {"id":"target","type":"agent","prompt":"Продолжи","after":[]}
  ]}`)
	historyID, sourceID := initial.Meta.Visits[0].VisitID, initial.Meta.Visits[1].VisitID
	if err := run.ReserveVisits([]string{historyID, sourceID}); err != nil {
		t.Fatal(err)
	}
	completeAgentVisit(t, run, historyID, "chat-history", scheduler.Succeeded, "")
	completeAgentVisit(t, run, sourceID, "chat-source", scheduler.Failed, "сеть недоступна")
	advanced, err := run.AdvanceAgentGraph()
	if err != nil || len(advanced.CreatedVisits) != 1 || advanced.CreatedVisits[0].StepID != "checker" {
		t.Fatalf("after не создал checker: %+v, %v", advanced, err)
	}
	checker := advanced.CreatedVisits[0]
	prepared, err := prepareAgentVisits(run, root, capacity.Unlimited(), false, nil, nil)
	if err != nil || len(prepared.Work) != 1 || prepared.Work[0].VisitID != checker.VisitID {
		t.Fatalf("checker не подготовлен: %+v, %v", prepared, err)
	}
	t.Cleanup(func() { _ = releaseAgentWork(prepared.Work) })
	command := prepared.Work[0].Command
	runDir, err := filepath.EvalSymlinks(filepath.Join(root, initial.Meta.RunID))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		initial.Meta.RunID, checker.VisitID, "Сделать MVP", "проверить границы", "Разбери результат",
		"Номер посещения кубика (visit): 1", "Итерация графа (iteration): 1", "(attempt): 1",
		"step=source", "state=failed", "техническая диагностика=сеть недоступна",
		filepath.Join(runDir, "memory", sourceID+".md"), "step=history", "state=succeeded",
		filepath.Join(runDir, "memory", historyID+".md"), filepath.Join(runDir, "memory", checker.VisitID+".md"),
		`["continue","stop"]`, "ровно один раз вызови встроенный choose_decision",
	} {
		if !strings.Contains(command.Text, fragment) {
			t.Errorf("visit-aware prompt не содержит %q\n%s", fragment, command.Text)
		}
	}
	profile := command.Permissions
	if profile == nil || profile.Name != "lawa-"+checker.VisitID || !reflect.DeepEqual(profile.ReadPaths, []string{runDir}) ||
		!reflect.DeepEqual(profile.WritePaths, []string{filepath.Join(runDir, "memory", checker.VisitID+".md")}) {
		t.Fatalf("visit получил неверные права памяти: %+v", profile)
	}
}

// TestChooseDecisionComposesWithConfiguredTools проверяет машинную границу
// решения целиком: child handler сохраняется, enum детерминирован, неверные JSON
// не меняют meta, а повторная доставка одного callId возвращает тот же commit.
func TestChooseDecisionComposesWithConfiguredTools(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"tools","start":["choice"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "stop":{"finish":"succeeded"},"go":{"to":["target"]},"abort":{"finish":"failed"}}},
    {"id":"target","type":"agent","prompt":"Работай","after":[]}
  ]}`)
	delegated := 0
	configure := func(_ runstore.Snapshot, command *codex.Command) {
		command.DynamicTools = []codex.DynamicTool{{
			Name: "run_child", Description: "Запустить ребёнка.", InputSchema: []byte(`{"type":"object"}`),
		}}
		command.CallDynamicTool = func(_ context.Context, call codex.DynamicToolCall) (string, error) {
			delegated++
			return "child:" + call.Tool, nil
		}
	}
	prepared, err := prepareAgentVisits(run, root, capacity.Unlimited(), false, nil, configure)
	if err != nil || len(prepared.Work) != 1 {
		t.Fatalf("decision visit не подготовлен: %+v, %v", prepared, err)
	}
	t.Cleanup(func() { _ = releaseAgentWork(prepared.Work) })
	command := prepared.Work[0].Command
	if len(command.DynamicTools) != 2 || command.DynamicTools[0].Name != "run_child" || command.DynamicTools[1].Name != chooseDecisionToolName {
		t.Fatalf("configure tools потеряны или продублированы: %+v", command.DynamicTools)
	}
	var schema struct {
		AdditionalProperties bool `json:"additionalProperties"`
		Properties           struct {
			Decision struct {
				Enum []string `json:"enum"`
			} `json:"decision"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(command.DynamicTools[1].InputSchema, &schema); err != nil || schema.AdditionalProperties ||
		!reflect.DeepEqual(schema.Required, []string{"decision"}) || !reflect.DeepEqual(schema.Properties.Decision.Enum, []string{"abort", "go", "stop"}) {
		t.Fatalf("неверная strict schema: %+v, %v", schema, err)
	}
	childResult, err := command.CallDynamicTool(t.Context(), codex.DynamicToolCall{Tool: "run_child"})
	if err != nil || childResult != "child:run_child" || delegated != 1 {
		t.Fatalf("исходный handler вызван неверно: %q, calls=%d, %v", childResult, delegated, err)
	}

	visitID := initial.Meta.Visits[0].VisitID
	if err := run.UpdateVisit(visitID, scheduler.Unknown, "chat-choice", ""); err == nil {
		err = run.SetVisitTurn(visitID, "turn-choice")
		if err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}
	invalid := []codex.DynamicToolCall{
		{ThreadID: "chat-choice", TurnID: "turn-choice", CallID: "bad-missing", Tool: chooseDecisionToolName, Arguments: []byte(`{}`)},
		{ThreadID: "chat-choice", TurnID: "turn-choice", CallID: "bad-extra", Tool: chooseDecisionToolName, Arguments: []byte(`{"decision":"go","extra":true}`)},
		{ThreadID: "chat-choice", TurnID: "turn-choice", CallID: "bad-duplicate", Tool: chooseDecisionToolName, Arguments: []byte(`{"decision":"go","decision":"stop"}`)},
		{ThreadID: "chat-choice", TurnID: "turn-choice", CallID: "bad-trailing", Tool: chooseDecisionToolName, Arguments: []byte(`{"decision":"go"}{}`)},
		{ThreadID: "chat-choice", TurnID: "turn-choice", CallID: "bad-key", Tool: chooseDecisionToolName, Arguments: []byte(`{"decision":"unknown"}`)},
		{ThreadID: "other-chat", TurnID: "turn-choice", CallID: "bad-thread", Tool: chooseDecisionToolName, Arguments: []byte(`{"decision":"go"}`)},
	}
	for _, call := range invalid {
		if _, callErr := command.CallDynamicTool(t.Context(), call); callErr == nil {
			t.Fatalf("принят неверный choose_decision: %+v", call)
		}
	}
	if saved, loadErr := run.Load(); loadErr != nil || saved.Meta.Visits[0].Decision != nil {
		t.Fatalf("отказ tool изменил meta: %+v, %v", saved.Meta.Visits[0].Decision, loadErr)
	}
	call := codex.DynamicToolCall{
		ThreadID: "chat-choice", TurnID: "turn-choice", CallID: "call-choice", Tool: chooseDecisionToolName,
		Arguments: []byte(`{"decision":"go","explanation":"нужна следующая работа"}`),
	}
	first, err := command.CallDynamicTool(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	second, err := command.CallDynamicTool(t.Context(), call)
	if err != nil || first != second || !strings.Contains(first, `"committed":true`) || !strings.Contains(first, `"to":["target"]`) {
		t.Fatalf("durable replay не подтверждён: %q / %q, %v", first, second, err)
	}
	saved, err := run.Load()
	record := saved.Meta.Visits[0].Decision
	if err != nil || record == nil || record.Key != "go" || record.CallID != "call-choice" || record.Applied || record.Error != "" ||
		!reflect.DeepEqual(record.To, []string{"target"}) || !reflect.DeepEqual(record.Skipped, []string{"abort", "stop"}) {
		t.Fatalf("решение не сохранено ровно один раз: %+v, %v", record, err)
	}
}

// TestCancelledDecisionKeepsCommittedChoice не даёт resume попросить модель
// выбрать маршрут заново. Новый turn получает прежний чат, следующий attempt и
// только внешние tools; после Succeeded планировщик применит старый durable key.
func TestCancelledDecisionKeepsCommittedChoice(t *testing.T) {
	root, initial, run := createAgentPreparationRun(t, `{
  "version":2,"id":"resume-choice","start":["choice"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{
      "go":{"to":["target"]},"stop":{"finish":"failed"}}},
    {"id":"target","type":"agent","prompt":"Работай","after":[]}
  ]}`)
	visitID := initial.Meta.Visits[0].VisitID
	if err := run.ReserveVisits([]string{visitID}); err != nil {
		t.Fatal(err)
	}
	completeAgentVisit(t, run, visitID, "chat-choice", scheduler.Running, "")
	if _, err := run.CommitDecision(visitID, "chat-choice", "turn-"+visitID, "go", "продолжить", "call-choice"); err == nil {
		err = run.UpdateVisit(visitID, scheduler.Cancelled, "chat-choice", "остановлено")
		if err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}
	configure := func(_ runstore.Snapshot, command *codex.Command) {
		command.DynamicTools = []codex.DynamicTool{{Name: "run_child", Description: "Запустить ребёнка.", InputSchema: []byte(`{"type":"object"}`)}}
		command.CallDynamicTool = func(context.Context, codex.DynamicToolCall) (string, error) { return "child", nil }
	}
	prepared, err := prepareAgentVisits(run, root, capacity.Unlimited(), true, map[string]bool{}, configure)
	if err != nil || len(prepared.Work) != 1 {
		t.Fatalf("Cancelled choice не продолжен: %+v, %v", prepared, err)
	}
	t.Cleanup(func() { _ = releaseAgentWork(prepared.Work) })
	work := prepared.Work[0]
	if work.kind != agentWorkContinuation || work.ThreadID != "chat-choice" || len(work.Command.DynamicTools) != 1 || work.Command.DynamicTools[0].Name != "run_child" ||
		!strings.Contains(work.Command.Text, "(attempt): 2") || !strings.Contains(work.Command.Text, `Решение уже устойчиво сохранено предыдущим turn: "go"`) ||
		!strings.Contains(work.Command.Text, "Сохранённое состояние перед новым turn: cancelled") ||
		!strings.Contains(work.Command.Text, "Техническая диагностика предыдущего turn этого посещения: остановлено") ||
		!strings.Contains(work.Command.Text, "Сохранённое объяснение решения: продолжить") || !strings.Contains(work.Command.Text, "Не вызывай choose_decision повторно") {
		t.Fatalf("resume потерял durable choice или добавил новый tool: %+v\n%s", work, work.Command.Text)
	}
}

// TestPrepareAgentVisitsRejectsWrongRootLegacyAndToolCollision закрепляет
// production gate и локальные ошибки до Reserve. Ни чужой root, ни v1 snapshot,
// ни занятое служебное имя не должны оставить Pending visit в Starting.
func TestPrepareAgentVisitsRejectsWrongRootLegacyAndToolCollision(t *testing.T) {
	definition := `{
  "version":2,"id":"guard","start":["choice"],"steps":[
    {"id":"choice","type":"agent","prompt":"Выбери","after":[],"decisions":{"done":{"finish":"succeeded"}}}
  ]}`
	root, initial, run := createAgentPreparationRun(t, definition)
	if _, err := prepareAgentVisits(run, t.TempDir(), capacity.Unlimited(), false, nil, nil); err == nil {
		t.Fatal("чужой root принят")
	}
	// Пустого чужого root недостаточно для регрессии split-brain: прежняя
	// реализация и так падала на отсутствующем memory-файле. Строим правдоподобный
	// двойник с тем же runId и всеми visitId — identity каталога всё равно должна
	// не совпасть, а полученный capacity slot обязан остаться свободным.
	lookalikeRoot := t.TempDir()
	lookalikeMemory := filepath.Join(lookalikeRoot, initial.Meta.RunID, "memory")
	if err := os.MkdirAll(lookalikeMemory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, visit := range initial.Meta.Visits {
		if err := os.WriteFile(filepath.Join(lookalikeMemory, visit.VisitID+".md"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := capacity.Configure(root, "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAgentVisits(run, lookalikeRoot, pool, false, nil, nil); err == nil || !strings.Contains(err.Error(), "не на открытый каталог") {
		t.Fatalf("правдоподобный чужой root принят: %v", err)
	}
	lease, available, err := pool.TryAcquire()
	if err != nil || !available {
		t.Fatalf("отказ чужому root утёк capacity slot: available=%v, %v", available, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	collision := func(_ runstore.Snapshot, command *codex.Command) {
		command.DynamicTools = []codex.DynamicTool{{Name: chooseDecisionToolName, Description: "Подмена.", InputSchema: []byte(`{"type":"object"}`)}}
		command.CallDynamicTool = func(context.Context, codex.DynamicToolCall) (string, error) { return "bad", nil }
	}
	if _, err := prepareAgentVisits(run, root, capacity.Unlimited(), false, nil, collision); err == nil || !strings.Contains(err.Error(), "служебное имя") {
		t.Fatalf("коллизия choose_decision не отклонена: %v", err)
	}
	withoutHandler := func(_ runstore.Snapshot, command *codex.Command) {
		command.DynamicTools = []codex.DynamicTool{{Name: "run_child", Description: "Без обработчика.", InputSchema: []byte(`{"type":"object"}`)}}
	}
	if _, err := prepareAgentVisits(run, root, capacity.Unlimited(), false, nil, withoutHandler); err == nil || !strings.Contains(err.Error(), "без обработчика") {
		t.Fatalf("dynamic tool без исходного handler не отклонён: %v", err)
	}
	saved, err := run.Load()
	if err != nil || saved.Meta.Visits[0].State != scheduler.Pending || saved.Meta.Visits[0].VisitID != initial.Meta.Visits[0].VisitID {
		t.Fatalf("локальная ошибка зарезервировала visit: %+v, %v", saved.Meta.Visits, err)
	}

	legacyRoot := t.TempDir()
	legacy, err := runstore.Create(legacyRoot, runstore.Input{
		WorkflowJSON: []byte(`{"id":"legacy","steps":[{"id":"one","type":"agent","prompt":"Один","dependsOn":[]}]}`),
		Task:         "Задача", CWD: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyRun, err := runstore.OpenLocked(legacyRoot, legacy.Meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer legacyRun.Close()
	if _, err := prepareAgentVisits(legacyRun, legacyRoot, capacity.Unlimited(), false, nil, nil); err == nil {
		t.Fatal("visit-aware prepare принял legacy run")
	}
	if _, err := prepareAgentVisits(nil, root, capacity.Unlimited(), false, nil, nil); err == nil {
		t.Fatal("nil LockedRun принят")
	}

	// Отдельно проверяем файл памяти: symlink не становится доверенным даже если
	// указывает на обычный файл. Load/prepare обязаны остановиться до reserve.
	memory := filepath.Join(root, initial.Meta.RunID, "memory", initial.Meta.Visits[0].VisitID+".md")
	if err := os.Remove(memory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, initial.Meta.RunID, "task.md"), memory); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAgentVisits(run, root, capacity.Unlimited(), false, nil, nil); err == nil {
		t.Fatal("symlink вместо памяти принят")
	}
}

// TestPrepareAgentVisitsRejectsSymlinkedMemoryDirectory проверяет предка всех
// write-путей. Даже если каждый файл в целевом каталоге обычный, симлинк memory
// не должен позволять вывести память агента из закреплённого дерева запуска.
func TestPrepareAgentVisitsRejectsSymlinkedMemoryDirectory(t *testing.T) {
	definition := `{
  "version":2,"id":"memory-parent","start":["one"],"steps":[
    {"id":"one","type":"agent","prompt":"Один","after":[]}
  ]}`
	root, initial, run := createAgentPreparationRun(t, definition)
	runMemory := filepath.Join(root, initial.Meta.RunID, "memory")
	// Цель находится внутри run, поэтому os.Root.Load способен пройти по ссылке.
	// Это гарантирует, что отказ принадлежит именно проверке permission path, а
	// не более ранней защите os.Root от выхода за границу запуска.
	externalMemory := filepath.Join(root, initial.Meta.RunID, "memory-target")
	if err := os.Mkdir(externalMemory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, visit := range initial.Meta.Visits {
		source := filepath.Join(runMemory, visit.VisitID+".md")
		target := filepath.Join(externalMemory, visit.VisitID+".md")
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(target, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(runMemory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(externalMemory), runMemory); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAgentVisits(run, root, capacity.Unlimited(), false, nil, nil); err == nil || !strings.Contains(err.Error(), "каталог памяти") {
		t.Fatalf("symlink-каталог memory принят: %v", err)
	}
	saved, err := run.Load()
	if err != nil || saved.Meta.Visits[0].State != scheduler.Pending {
		t.Fatalf("отказ symlink-каталогу зарезервировал visit: %+v, %v", saved.Meta.Visits, err)
	}
}
