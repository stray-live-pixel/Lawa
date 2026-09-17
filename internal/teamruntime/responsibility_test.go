package teamruntime

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stray-live-pixel/Lawa/internal/codex"
	"github.com/stray-live-pixel/Lawa/internal/runstore"
)

// assignedTeam оставляет Босса в активном ходе и регистрирует поручение. Так
// проверяется окно до пятиминутного claim, которое раньше позволяло потерять работу.
func assignedTeam(t *testing.T) (*Engine, string, *fakeClient, *time.Time) {
	t.Helper()
	e, run, client, now := teamEngine(t)
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := runstore.SummonDeveloper(t.Context(), e.Root, run, "boss"); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "assignment", "@developer Реализуй игру и проверь управление"); err != nil {
		t.Fatal(err)
	}
	return e, run, client, now
}

// Завершение запрещено и до запуска коллеги, и после его отчёта без приёмки.
// Проверяются также права, недоставленная новая задача и идемпотентность RPC.
func TestGoalRequiresExplicitAcceptance(t *testing.T) {
	e, run, _, now := assignedTeam(t)
	complete := func() error {
		_, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "done", "@human Игра проверена")
		return err
	}
	before := readChat(t, e, run)
	if err := complete(); err == nil {
		t.Fatal("поглощено ещё не начатое поручение")
	}
	if len(readChat(t, e, run).Messages) != len(before.Messages) {
		t.Fatal("частичная запись завершения")
	}
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", now.Add(6*time.Minute)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "developer", "report", "@boss Игра в index.html, управление проверено"); err != nil {
		t.Fatal(err)
	}
	accept := func(author, event string, ids []string, result, evidence string) error {
		_, err := runstore.AcceptTeamTasks(t.Context(), e.Root, run, author, event, ids, result, evidence)
		return err
	}
	if err := accept("boss", "accept", []string{"assignment"}, "report", "Проверил управление"); err == nil {
		t.Fatal("принят ещё выполняющийся сотрудник")
	}
	if err := e.finish(t.Context(), run, "developer"); err != nil {
		t.Fatal(err)
	}
	if err := complete(); err == nil {
		t.Fatal("ответ принят автоматически")
	}
	for _, in := range []struct {
		author, result, evidence string
		ids                      []string
	}{
		{"developer", "report", "Проверил", []string{"assignment"}},
		{"boss", "assignment", "Проверил", []string{"assignment"}},
		{"boss", "report", "", []string{"assignment"}},
		{"boss", "report", "Проверил", []string{"unknown"}},
		{"boss", "report", "Проверил", []string{"assignment", "assignment"}},
	} {
		if err := accept(in.author, "invalid", in.ids, in.result, in.evidence); err == nil {
			t.Fatal("принята неверная приёмка", in)
		}
	}
	if err := accept("boss", "accept", []string{"assignment"}, "report", "Запустил игру и проверил управление"); err != nil {
		t.Fatal(err)
	}
	count := len(readChat(t, e, run).Messages)
	if err := accept("boss", "accept", []string{"assignment"}, "report", "Запустил игру и проверил управление"); err != nil {
		t.Fatal(err)
	}
	if len(readChat(t, e, run).Messages) != count {
		t.Fatal("повторная приёмка создала событие")
	}
	// Новый вопрос тому же сотруднику не покрывается старым проверенным отчётом.
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "followup", "@developer Проверь перезапуск"); err != nil {
		t.Fatal(err)
	}
	if err := accept("boss", "stale", []string{"followup"}, "report", "Проверил"); err == nil {
		t.Fatal("старый отчёт закрыл новое поручение")
	}
	if err := complete(); err == nil {
		t.Fatal("новое поручение поглощено")
	}
}

// Уведомления о сбое и молчаливом final адресованы Боссу и доставляются сразу.
// Поручение остаётся открытым; повтор сохранения ошибки не множит уведомления.
func TestEmployeeProblemWakesBoss(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "без отчёта", true: "сбой"}[failure], func(t *testing.T) {
			e, run, client, now := assignedTeam(t)
			if err := e.finish(t.Context(), run, "boss"); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(6 * time.Minute)
			client.execute = func(_ context.Context, c codex.Command) (codex.Result, error) {
				if failure {
					return codex.Result{}, errors.New("сбой до отправки")
				}
				start(t, c, "developer-thread", "developer-turn")
				return codex.Result{Status: "completed"}, nil
			}
			if err := e.Process(t.Context(), run, "developer"); (err != nil) != failure {
				t.Fatal(err)
			}
			chat := readChat(t, e, run)
			last := chat.Messages[len(chat.Messages)-1]
			if last.AuthorID != "system" || last.To != "boss" || chat.Room.Tasks["assignment"].AcceptedAt != nil {
				t.Fatal(chat)
			}
			if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || !ok {
				t.Fatal("Босс не проснулся", ok, err)
			}
			if failure {
				if err := e.block(t.Context(), run, "developer", "тот же сбой", false); err != nil {
					t.Fatal(err)
				}
				if len(readChat(t, e, run).Messages) != len(chat.Messages) {
					t.Fatal("дублирован сбой")
				}
			}
		})
	}
}

// Новый thread и старый team_post выполняют одну приёмку. После перезапуска
// принятый результат сохранён; повтор команды не создаёт событие или новый turn.
func TestAcceptanceToolsAndLegacyPersistence(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "tool", true: "legacy"}[legacy], func(t *testing.T) {
			e, run, _, now := assignedTeam(t)
			if _, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", now.Add(6*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := runstore.PostActor(t.Context(), e.Root, run, "developer", "report", "@boss Готово, тесты пройдены"); err != nil {
				t.Fatal(err)
			}
			if err := e.finish(t.Context(), run, "developer"); err != nil {
				t.Fatal(err)
			}
			args := `{"task_ids":["assignment"],"result_id":"report","evidence":"Открыл игру и проверил управление"}`
			call := codex.DynamicToolCall{Tool: "team_accept", CallID: "accept", Arguments: []byte(args)}
			if legacy {
				call.Tool = "team_post"
				call.Arguments, _ = json.Marshal(map[string]string{"text": "/accept " + args})
			}
			if _, handled, err := e.controlTool(t.Context(), run, "developer", call); err == nil || !handled {
				t.Fatal("нарушены права", handled, err)
			}
			if _, handled, err := e.controlTool(t.Context(), run, "boss", call); err != nil || !handled {
				t.Fatal(handled, err)
			}
			again := &Engine{Root: e.Root}
			if _, handled, err := again.controlTool(t.Context(), run, "boss", call); err != nil || !handled {
				t.Fatal(handled, err)
			}
			if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "complete", "@human Игра проверена"); err != nil {
				t.Fatal(err)
			}
			if readChat(t, e, run).Room.AchievedAt == nil {
				t.Fatal("цель не закрыта после приёмки")
			}
		})
	}
}

// Босс может исправить подтверждённый сбой до отправки. Неоднозначный результат
// сети остаётся заблокированным, независимо от желания Босса и legacy-команды.
func TestBossRetriesOnlyUnsentAndExplainsBlockedRelay(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(map[bool]string{false: "не отправлено", true: "неоднозначно"}[attempted], func(t *testing.T) {
			e, run, client, now := assignedTeam(t)
			if err := e.finish(t.Context(), run, "boss"); err != nil {
				t.Fatal(err)
			}
			postHuman(t, e, run, "human-request", "@developer Где результат?")
			*now = now.Add(time.Minute)
			client.execute = func(context.Context, codex.Command) (codex.Result, error) {
				return codex.Result{CreationAttempted: attempted}, errors.New("сбой")
			}
			if err := e.Process(t.Context(), run, "developer"); err == nil {
				t.Fatal("нет ошибки")
			}
			if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "explain", "@human Сотрудник заблокирован, проверяю причину"); err != nil {
				t.Fatal("Босс не может сообщить препятствие", err)
			}
			call := codex.DynamicToolCall{Tool: "team_post", CallID: "retry", Arguments: []byte(`{"text":"/retry developer"}`)}
			_, _, err := e.controlTool(t.Context(), run, "boss", call)
			if (err != nil) != attempted {
				t.Fatal(err)
			}
			if !attempted {
				count := len(readChat(t, e, run).Messages)
				if _, _, err := e.controlTool(t.Context(), run, "boss", call); err != nil {
					t.Fatal(err)
				}
				if len(readChat(t, e, run).Messages) != count {
					t.Fatal("retry не идемпотентен")
				}
				if readChat(t, e, run).Room.Actors["developer"].Delivery != nil {
					t.Fatal("доставка не освобождена")
				}
			}
			if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "complete", "@human Готово"); err == nil {
				t.Fatal("проблемное поручение закрыто")
			}
		})
	}
}

// Миграция активной комнаты не теряет выданную работу и не пишет при GET.
// Уже достигнутая старая цель не открывается заново из-за нового формата.
func TestLegacyTasksDerivedWithoutWriting(t *testing.T) {
	e, run, _, _ := assignedTeam(t)
	chat := readChat(t, e, run)
	chat.Room.Tasks = nil
	data, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Root, run, "team.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded := readChat(t, e, run)
	if len(loaded.Room.Tasks) != 1 || loaded.Room.Tasks["assignment"].AcceptedAt != nil {
		t.Fatal("потеряна работа", loaded.Room.Tasks)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("GET изменил файл", err)
	}
	now := time.Now().UTC()
	chat.Room.AchievedAt = &now
	chat.Messages = append(chat.Messages, runstore.TeamMessage{ID: "old-done", AuthorID: "system", Kind: "achievement", Date: now}, runstore.TeamMessage{ID: "old-final", AuthorID: "boss", To: "human", Date: now, Text: "@human Готово"})
	data, err = json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if len(readChat(t, e, run).Room.Tasks) != 0 {
		t.Fatal("повторно открыта старая работа")
	}
}

// Отчёт может попасть в ход Босса до завершения хода сотрудника. После отказа
// приёмки нужен новый адресный сигнал, иначе Босс навсегда останется ждать.
func TestEmployeeFinishNotifiesBossAfterEarlyReport(t *testing.T) {
	e, run, _, now := assignedTeam(t)
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(6 * time.Minute)
	if _, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", *now); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "developer", "report", "@boss Готово"); err != nil {
		t.Fatal(err)
	}
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := runstore.AcceptTeamTasks(t.Context(), e.Root, run, "boss", "early", []string{"assignment"}, "report", "Проверил"); err == nil {
		t.Fatal("ранняя приёмка")
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	if err := e.finish(t.Context(), run, "developer"); err != nil {
		t.Fatal(err)
	}
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || !ok {
		t.Fatal("нет повторного сигнала", ok, err)
	}
}

// Повторный доказанный сбой после решения Босса — новое событие, даже если
// очередь поручений та же. Дедупликация одной попытки не должна скрыть следующую.
func TestRepeatedUnsentFailureNotifiesAgain(t *testing.T) {
	e, run, client, now := assignedTeam(t)
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(6 * time.Minute)
	client.execute = func(context.Context, codex.Command) (codex.Result, error) {
		return codex.Result{}, errors.New("сеть недоступна до отправки")
	}
	if err := e.Process(t.Context(), run, "developer"); err == nil {
		t.Fatal("нет сбоя")
	}
	first := readChat(t, e, run)
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := e.retryByBoss(t.Context(), run, "retry", "developer"); err != nil {
		t.Fatal(err)
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	if err := e.Process(t.Context(), run, "developer"); err == nil {
		t.Fatal("нет повторного сбоя")
	}
	after := readChat(t, e, run)
	last := after.Messages[len(after.Messages)-1]
	if last.To != "boss" || last.ID == first.Messages[len(first.Messages)-1].ID {
		t.Fatal("повторный сбой потерян", last)
	}
	if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "boss", *now); err != nil || !ok {
		t.Fatal("нет повторного пробуждения", ok, err)
	}
}

// Общение и достижение цели — разные действия. Пока коллега ещё не ответил,
// Босс может уточнить вопрос Чела или сообщить о ходе работы. Это не принимает
// поручение и не даёт закрыть цель ни до claim сотрудника, ни во время его хода.
func TestBossCanMessageHumanWhileEmployeeHasNotAnswered(t *testing.T) {
	for _, working := range []bool{false, true} {
		t.Run(map[bool]string{false: "поручение ожидает", true: "сотрудник работает"}[working], func(t *testing.T) {
			e, run, _, now := assignedTeam(t)
			postHuman(t, e, run, "human-question", "@developer Проверь управление")
			if working {
				if ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, "developer", now.Add(6*time.Minute)); err != nil || !ok {
					t.Fatal(ok, err)
				}
			}
			if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "clarification", "@human Уточни, нужно ли управление с телефона?"); err != nil {
				t.Fatal("Босс не может уточнить вопрос", err)
			}
			if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "progress", "@human Разработчик проверяет управление"); err != nil {
				t.Fatal("Босс не может сообщить статус", err)
			}
			if readChat(t, e, run).Room.Tasks["human-question"].AcceptedAt != nil {
				t.Fatal("сообщение Босса приняло поручение")
			}
			if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "premature", "@human Готово"); err == nil {
				t.Fatal("общение позволило закрыть незавершённую цель")
			}
		})
	}
}

// Отчёт по повторному запросу относится к уже сделанной работе. Босс принимает
// исходное поручение Чела и повторный запрос, после чего может передать результат
// Челу и завершить цель. Совпадение replyTo с самым первым вопросом не требуется.
func TestRecoveredReportAllowsHumanReplyAndCompletion(t *testing.T) {
	e, run, _, now := assignedTeam(t)
	postHuman(t, e, run, "human-question", "@developer Как устроено управление?")
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(6 * time.Minute)
	claim := func(id string) {
		t.Helper()
		ok, err := runstore.ClaimTeamDelivery(t.Context(), e.Root, run, id, *now)
		if err != nil || !ok {
			t.Fatal(id, ok, err)
		}
	}
	claim("developer")
	if err := e.finish(t.Context(), run, "developer"); err != nil {
		t.Fatal(err)
	}
	claim("boss")
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "report-request", "@developer Пришли отчёт по игре и ответ Челу"); err != nil {
		t.Fatal(err)
	}
	if err := e.finish(t.Context(), run, "boss"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(6 * time.Minute)
	claim("developer")
	report, err := runstore.PostActor(t.Context(), e.Root, run, "developer", "recovered-report", "@boss Игра готова, стрелки перемещают героя, пробел задаёт прыжок. Проверил")
	if err != nil {
		t.Fatal(err)
	}
	if report.ReplyTo != "report-request" {
		t.Fatal("проверка должна воспроизводить ответ на повторное поручение", report)
	}
	if err := e.finish(t.Context(), run, "developer"); err != nil {
		t.Fatal(err)
	}
	claim("boss")
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "unaccepted", "@human Готово"); err == nil {
		t.Fatal("отчёт без приёмки закрыл цель")
	}
	if _, err := runstore.AcceptTeamTasks(t.Context(), e.Root, run, "boss", "accept-all", []string{"assignment", "human-question", "report-request"}, "recovered-report", "Открыл игру, сверил управление с исходниками"); err != nil {
		t.Fatal(err)
	}
	if _, err := runstore.PostActor(t.Context(), e.Root, run, "boss", "human-answer", "@human Стрелки перемещают героя, пробел задаёт прыжок. Проверено"); err != nil {
		t.Fatal("принятый результат нельзя передать Челу", err)
	}
	if _, err := runstore.CompleteTeam(t.Context(), e.Root, run, "boss", "goal-complete", "@human Цель достигнута, управление проверено"); err != nil {
		t.Fatal("нельзя завершить проверенную цель", err)
	}
	chat := readChat(t, e, run)
	if chat.Room.AchievedAt == nil || chat.Messages[len(chat.Messages)-1].To != "human" {
		t.Fatal("нет финала для Чела")
	}
}
