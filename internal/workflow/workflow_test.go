package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

const valid = `{"id":"flow","steps":[{"id":"a","type":"agent","prompt":"  задача\n","dependsOn":[]}]}`

const validV2 = `{"version":2,"id":"risk","model":"gpt-5.6-luna","start":["checker"],"steps":[` +
	`{"id":"checker","type":"agent","prompt":"Оцени риск.","after":[],"decisions":{` +
	`"risk":{"to":["audit","notify"]},"safe":{"finish":"succeeded"},"fail":{"finish":"failed"}},` +
	`"maxVisits":3,"onLimit":"failed","effort":"high","speed":"fast"},` +
	`{"id":"audit","type":"agent","prompt":"Проведи аудит.","after":[]},` +
	`{"id":"notify","type":"agent","prompt":"Подготовь уведомление.","after":[]}]} `

// TestDecode защищает точное сохранение входа: пробелы промпта и порядок шагов
// не исправляются валидатором, а пример содержит обратный порядок зависимостей.
func TestDecode(t *testing.T) {
	w, err := Decode(strings.NewReader(valid))
	if err != nil || w.Steps[0].Prompt != "  задача\n" {
		t.Fatalf("вход изменён или отклонён: %+v, %v", w, err)
	}
	f, err := os.Open("../../examples/review.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err = Decode(f)
	if err != nil || len(w.Steps) != 4 || w.Steps[0].ID != "summary" {
		t.Fatalf("пример с параллельными ветками отклонён: %+v, %v", w, err)
	}
}

// TestLegacyVersionCompatibility фиксирует обе записи прежнего контракта:
// исторический JSON без version не получает новое поле при кодировании, а явная
// version=1 сохраняется. Срез dependsOn=[] также нельзя потерять через omitzero,
// иначе повторное чтение ошибочно решит, что обязательного поля не было.
func TestLegacyVersionCompatibility(t *testing.T) {
	inputs := []string{
		valid,
		strings.Replace(valid, `{"id":"flow"`, `{"version":1,"id":"flow"`, 1),
	}
	for _, input := range inputs {
		w, err := Decode(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if w.EffectiveVersion() != VersionLegacy || (w.Version == nil) != !strings.Contains(input, `"version"`) {
			t.Fatalf("потеряно различие отсутствующей и явной legacy-версии: %+v", w.Version)
		}
		encoded, err := json.Marshal(w)
		if err != nil || string(encoded) != input {
			t.Fatalf("legacy JSON изменился: %s, %v; ожидался %s", encoded, err, input)
		}
	}
}

// TestDecodeAgentGraph проверяет основной if/else-контракт и fan-out одного
// решения. Порядок to сохраняется, потому что будущий runtime обязан планировать
// параллельную волну воспроизводимо, не обходя map решений.
func TestDecodeAgentGraph(t *testing.T) {
	w, err := Decode(strings.NewReader(strings.TrimSpace(validV2)))
	if err != nil {
		t.Fatal(err)
	}
	if w.EffectiveVersion() != VersionAgentGraph || !reflect.DeepEqual(w.Start, []string{"checker"}) || len(w.Steps) != 3 {
		t.Fatalf("искажён корень v2: %+v", w)
	}
	checker := w.Steps[0]
	if !reflect.DeepEqual(checker.Decisions["risk"].To, []string{"audit", "notify"}) ||
		checker.MaxVisits == nil || *checker.MaxVisits != 3 || checker.OnLimit == nil || *checker.OnLimit != OutcomeFailed ||
		checker.Effort == nil || *checker.Effort != "high" || checker.Speed == nil || *checker.Speed != SpeedFast {
		t.Fatalf("решения, лимит или runtime-настройки потеряны: %+v", checker)
	}
	encoded, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Decode(bytes.NewReader(encoded))
	if err != nil || !reflect.DeepEqual(w, again) {
		t.Fatalf("v2 не пережил повторное кодирование: %+v, %v", again, err)
	}
}

// TestAgentGraphAllowsBoundedDecisionCycle воспроизводит цикл из issue: start
// даёт первое посещение test, terminal fix удовлетворяет его after при повторах,
// а checker выбирает возврат либо явное успешное завершение.
func TestAgentGraphAllowsBoundedDecisionCycle(t *testing.T) {
	input := `{"version":2,"id":"repair","start":["test"],"steps":[` +
		`{"id":"test","type":"agent","prompt":"Тестируй.","after":["fix"]},` +
		`{"id":"checker","type":"agent","prompt":"Проверь.","after":["test"],"maxVisits":4,"decisions":{` +
		`"repeat":{"to":["fix"]},"done":{"finish":"succeeded"},"fail":{"finish":"failed"}}},` +
		`{"id":"fix","type":"agent","prompt":"Исправь.","after":[]}]}`
	if _, err := Decode(strings.NewReader(input)); err != nil {
		t.Fatalf("ограниченный decision-цикл отклонён: %v", err)
	}
}

// TestAgentGraphRejectsStaticAfterCycle подтверждает важную границу контракта:
// start не превращает обычный after-цикл в допустимый; замыкать его может только
// route агентного решения с отдельным maxVisits.
func TestAgentGraphRejectsStaticAfterCycle(t *testing.T) {
	input := `{"version":2,"id":"bad","start":["a"],"steps":[` +
		`{"id":"a","type":"agent","prompt":"A","after":["b"]},` +
		`{"id":"b","type":"agent","prompt":"B","after":["a"]}]}`
	if _, err := Decode(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "цикл after") {
		t.Fatalf("обычный after-цикл принят или плохо объяснён: %v", err)
	}
}

// TestWorkflowVersionsCannotBeMixed проверяет запрет неоднозначных полей в обе
// стороны. Даже пустой массив явно принадлежит своему контракту и не игнорируется.
func TestWorkflowVersionsCannotBeMixed(t *testing.T) {
	cases := map[string]string{
		"неизвестная версия": `{"version":3,"id":"bad","steps":[{"id":"a","type":"agent","prompt":"A","dependsOn":[]}]}`,
		"dependsOn в v2":     `{"version":2,"id":"bad","start":["a"],"steps":[{"id":"a","type":"agent","prompt":"A","after":[],"dependsOn":[]}]}`,
		"after без version":  `{"id":"bad","steps":[{"id":"a","type":"agent","prompt":"A","dependsOn":[],"after":[]}]}`,
		"start в v1":         `{"version":1,"id":"bad","start":[],"steps":[{"id":"a","type":"agent","prompt":"A","dependsOn":[]}]}`,
		"decisions в v1":     `{"version":1,"id":"bad","steps":[{"id":"a","type":"agent","prompt":"A","dependsOn":[],"decisions":{}}]}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("смешанный контракт принят")
			}
		})
	}
}

// TestAgentGraphReferences проверяет каждый вид ссылки и повторов. Ошибка должна
// возникнуть до анализа достижимости, чтобы автор видел локальную опечатку.
func TestAgentGraphReferences(t *testing.T) {
	base := `{"version":2,"id":"refs","start":["a"],"steps":[` +
		`{"id":"a","type":"agent","prompt":"A","after":[],"decisions":{"go":{"to":["b"]}}},` +
		`{"id":"b","type":"agent","prompt":"B","after":[]}]}`
	cases := map[string]string{
		"неизвестный start": strings.Replace(base, `"start":["a"]`, `"start":["missing"]`, 1),
		"повторный start":   strings.Replace(base, `"start":["a"]`, `"start":["a","a"]`, 1),
		"неизвестный after": strings.Replace(base, `"after":[]}]}`, `"after":["missing"]}]}`, 1),
		"повторный after":   strings.Replace(base, `"after":[]}]}`, `"after":["a","a"]}]}`, 1),
		"неизвестный to":    strings.Replace(base, `"to":["b"]`, `"to":["missing"]`, 1),
		"повторный to":      strings.Replace(base, `"to":["b"]`, `"to":["b","b"]`, 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("некорректная ссылка принята")
			}
		})
	}
}

// TestAgentGraphReachabilityAndExit не разрешает молча хранить никогда не
// активируемый кубик и цикл, у которого нет ни естественного листа, ни явного
// finish/onLimit. maxVisits остаётся предохранителем, а не маршрутом завершения.
func TestAgentGraphReachabilityAndExit(t *testing.T) {
	unreachable := `{"version":2,"id":"unreachable","start":["a"],"steps":[` +
		`{"id":"a","type":"agent","prompt":"A","after":[]},` +
		`{"id":"orphan","type":"agent","prompt":"X","after":[]}]}`
	if _, err := Decode(strings.NewReader(unreachable)); err == nil || !strings.Contains(err.Error(), "недостижим") {
		t.Fatalf("недостижимый шаг принят или плохо объяснён: %v", err)
	}
	unsafe := `{"version":2,"id":"unsafe","start":["loop"],"steps":[` +
		`{"id":"loop","type":"agent","prompt":"Повтори.","after":[],"maxVisits":2,"decisions":{"again":{"to":["loop"]}}}]}`
	if _, err := Decode(strings.NewReader(unsafe)); err == nil || !strings.Contains(err.Error(), "не имеет пути") {
		t.Fatalf("граф без безопасного выхода принят или плохо объяснён: %v", err)
	}
	withLimitOutcome := strings.Replace(unsafe, `"maxVisits":2`, `"maxVisits":2,"onLimit":"failed"`, 1)
	if _, err := Decode(strings.NewReader(withLimitOutcome)); err != nil {
		t.Fatalf("явный terminal onLimit не признан безопасным выходом: %v", err)
	}
}

// TestAgentGraphRejectsInvalidRoutesAndLimits покрывает семантику, которую нельзя
// выразить одними Go-типами: пустые формы, два исхода сразу и несвязанный onLimit.
func TestAgentGraphRejectsInvalidRoutesAndLimits(t *testing.T) {
	step := func(extra string) string {
		return `{"version":2,"id":"invalid","start":["a"],"steps":[{"id":"a","type":"agent","prompt":"A","after":[]` + extra + `}]}`
	}
	cases := map[string]string{
		"нет start":               `{"version":2,"id":"invalid","steps":[{"id":"a","type":"agent","prompt":"A","after":[]}]}`,
		"пустой start":            `{"version":2,"id":"invalid","start":[],"steps":[{"id":"a","type":"agent","prompt":"A","after":[]}]}`,
		"нет after":               `{"version":2,"id":"invalid","start":["a"],"steps":[{"id":"a","type":"agent","prompt":"A"}]}`,
		"пустые decisions":        step(`,"decisions":{}`),
		"пустой ключ":             step(`,"decisions":{" ":{"finish":"failed"}}`),
		"нет формы route":         step(`,"decisions":{"bad":{}}`),
		"пустой to":               step(`,"decisions":{"bad":{"to":[]}}`),
		"две формы route":         step(`,"decisions":{"bad":{"to":["a"],"finish":"failed"}},"maxVisits":1,"onLimit":"failed"`),
		"неверный finish":         step(`,"decisions":{"bad":{"finish":"cancelled"}}`),
		"нулевой maxVisits":       step(`,"maxVisits":0`),
		"отрицательный maxVisits": step(`,"maxVisits":-1`),
		"onLimit без лимита":      step(`,"onLimit":"failed"`),
		"неверный onLimit":        step(`,"maxVisits":1,"onLimit":"cancelled"`),
		"цикл без лимита":         step(`,"decisions":{"repeat":{"to":["a"]},"stop":{"finish":"failed"}}`),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("некорректный route или лимит принят")
			}
		})
	}
}

// TestEveryCyclicDecisionRequiresLimit строит одну SCC с двумя агентными
// решениями. Лимит только на одном из них недостаточен: другой decision обязан
// иметь собственный статический предохранитель независимо от выбранного route.
func TestEveryCyclicDecisionRequiresLimit(t *testing.T) {
	input := `{"version":2,"id":"limits","start":["first"],"steps":[` +
		`{"id":"first","type":"agent","prompt":"Первый.","after":[],"maxVisits":2,"decisions":{` +
		`"next":{"to":["second"]},"stop":{"finish":"failed"}}},` +
		`{"id":"second","type":"agent","prompt":"Второй.","after":[],"decisions":{` +
		`"back":{"to":["first"]},"stop":{"finish":"succeeded"}}}]}`
	if _, err := Decode(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), `"second"`) || !strings.Contains(err.Error(), "maxVisits") {
		t.Fatalf("decision без собственного лимита принят или плохо объяснён: %v", err)
	}
}

// TestAgentGraphStrictJSON распространяет прежний строгий JSON-контракт на все
// новые уровни. null не равен отсутствию, а повторный ключ решения не выбирается
// библиотекой произвольно.
func TestAgentGraphStrictJSON(t *testing.T) {
	base := `{"version":2,"id":"strict","start":["a"],"steps":[{"id":"a","type":"agent","prompt":"A","after":[]}]}`
	cases := map[string]string{
		"null version":        strings.Replace(base, `"version":2`, `"version":null`, 1),
		"null start":          strings.Replace(base, `"start":["a"]`, `"start":null`, 1),
		"null after":          strings.Replace(base, `"after":[]`, `"after":null`, 1),
		"null decisions":      strings.Replace(base, `"after":[]`, `"after":[],"decisions":null`, 1),
		"null maxVisits":      strings.Replace(base, `"after":[]`, `"after":[],"maxVisits":null`, 1),
		"null onLimit":        strings.Replace(base, `"after":[]`, `"after":[],"onLimit":null`, 1),
		"null route":          strings.Replace(base, `"after":[]`, `"after":[],"decisions":{"x":null}`, 1),
		"null route to":       strings.Replace(base, `"after":[]`, `"after":[],"decisions":{"x":{"to":null}}`, 1),
		"null route finish":   strings.Replace(base, `"after":[]`, `"after":[],"decisions":{"x":{"finish":null}}`, 1),
		"unknown route field": strings.Replace(base, `"after":[]`, `"after":[],"decisions":{"x":{"other":true}}`, 1),
		"duplicate decision":  strings.Replace(base, `"after":[]`, `"after":[],"decisions":{"x":{"finish":"failed"},"x":{"finish":"succeeded"}}`, 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			w, err := Decode(strings.NewReader(input))
			if err == nil || !reflect.DeepEqual(w, Workflow{}) {
				t.Fatalf("нестрогий JSON принят: %+v, %v", w, err)
			}
		})
	}
}

// TestDecisionDiagnosticsAreDeterministic доказывает, что случайный порядок map
// не меняет первую ошибку: ключи решений проверяются лексикографически.
func TestDecisionDiagnosticsAreDeterministic(t *testing.T) {
	prefix := `{"version":2,"id":"order","start":["a"],"steps":[{"id":"a","type":"agent","prompt":"A","after":[],"decisions":`
	suffix := `}]}`
	first := prefix + `{"z":{"to":["missing-z"]},"a":{"to":["missing-a"]}}` + suffix
	second := prefix + `{"a":{"to":["missing-a"]},"z":{"to":["missing-z"]}}` + suffix
	_, firstErr := Decode(strings.NewReader(first))
	_, secondErr := Decode(strings.NewReader(second))
	if firstErr == nil || secondErr == nil || firstErr.Error() != secondErr.Error() || !strings.Contains(firstErr.Error(), "missing-a") {
		t.Fatalf("диагностика зависит от порядка map: %v / %v", firstErr, secondErr)
	}
}

// TestDecodeRuntimeSettings проверяет независимую опциональность настроек. Их
// указатели сохраняют различие между наследованием Codex и явным override кубика.
func TestDecodeRuntimeSettings(t *testing.T) {
	input := `{"id":"runtime","model":"gpt-5.6-luna","steps":[` +
		`{"id":"model","type":"agent","prompt":"Модель","dependsOn":[],"model":"gpt-5.6-sol"},` +
		`{"id":"effort","type":"agent","prompt":"Рассуждение","dependsOn":[],"effort":"high"},` +
		`{"id":"speed","type":"agent","prompt":"Скорость","dependsOn":[],"speed":"fast"},` +
		`{"id":"inherited","type":"agent","prompt":"Обычная задача","dependsOn":[]}]}`
	w, err := Decode(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if w.Model == nil || *w.Model != "gpt-5.6-luna" {
		t.Fatalf("общая модель workflow потеряна: %+v", w.Model)
	}
	model, effort, speed, inherited := w.Steps[0], w.Steps[1], w.Steps[2], w.Steps[3]
	if model.Model == nil || *model.Model != "gpt-5.6-sol" || model.Effort != nil || model.Speed != nil ||
		effort.Model != nil || effort.Effort == nil || *effort.Effort != "high" || effort.Speed != nil ||
		speed.Model != nil || speed.Effort != nil || speed.Speed == nil || *speed.Speed != SpeedFast {
		t.Fatalf("независимые настройки изменены или потеряны: model=%+v effort=%+v speed=%+v", model, effort, speed)
	}
	if inherited.Model != nil || inherited.Effort != nil || inherited.Speed != nil {
		t.Fatalf("отсутствующие настройки перестали наследоваться: %+v", inherited)
	}
}

// TestDecodeRejectsInvalidInput проверяет отклонение всей схемы, включая попытки
// скрыть ошибку повторным ключом, другим регистром или вторым JSON-документом.
func TestDecodeRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"пустой ввод": "", "обрыв": `{"id":`, "массив вместо объекта": `[]`,
		"null вместо объекта": `null`, "второй объект": valid + `{}`, "мусор после": valid + `x`,
		"нет id":      strings.Replace(valid, `"id":"flow",`, "", 1),
		"пустой id":   strings.Replace(valid, `"flow"`, `"  "`, 1),
		"числовой id": strings.Replace(valid, `"flow"`, `42`, 1),
		"нет steps":   `{"id":"flow"}`, "пустые steps": `{"id":"flow","steps":[]}`,
		"null steps": `{"id":"flow","steps":null}`, "тип steps": `{"id":"flow","steps":{}}`,
		"null шага":                 `{"id":"flow","steps":[null]}`,
		"пустой id шага":            strings.Replace(valid, `"id":"a"`, `"id":" "`, 1),
		"нет id шага":               strings.Replace(valid, `"id":"a",`, "", 1),
		"нет type":                  strings.Replace(valid, `"type":"agent",`, "", 1),
		"неверный type":             strings.Replace(valid, `"agent"`, `"shell"`, 1),
		"нет prompt":                strings.Replace(valid, `"prompt":"  задача\n",`, "", 1),
		"пустой prompt":             strings.Replace(valid, `"  задача\n"`, `" \n"`, 1),
		"null prompt":               strings.Replace(valid, `"  задача\n"`, `null`, 1),
		"нет dependsOn":             strings.Replace(valid, `,"dependsOn":[]`, "", 1),
		"null dependsOn":            strings.Replace(valid, `"dependsOn":[]`, `"dependsOn":null`, 1),
		"тип dependsOn":             strings.Replace(valid, `"dependsOn":[]`, `"dependsOn":"a"`, 1),
		"тип ссылки":                strings.Replace(valid, `"dependsOn":[]`, `"dependsOn":[42]`, 1),
		"неизвестное поле":          strings.Replace(valid, `"id":"flow"`, `"other":1,"id":"flow"`, 1),
		"регистр поля":              strings.Replace(valid, `"dependsOn"`, `"DependsOn"`, 1),
		"повтор поля workflow":      strings.Replace(valid, `"id":"flow"`, `"id":"flow","id":"other"`, 1),
		"повтор поля шага":          strings.Replace(valid, `"id":"a"`, `"id":"a","\u0069d":"b"`, 1),
		"пустая model workflow":     strings.Replace(valid, `"id":"flow"`, `"id":"flow","model":""`, 1),
		"model workflow с пробелом": strings.Replace(valid, `"id":"flow"`, `"id":"flow","model":"bad model"`, 1),
		"числовая model workflow":   strings.Replace(valid, `"id":"flow"`, `"id":"flow","model":42`, 1),
		"пустая model":              strings.Replace(valid, `"prompt":`, `"model":"","prompt":`, 1),
		"model с пробелом":          strings.Replace(valid, `"prompt":`, `"model":"bad model","prompt":`, 1),
		"числовая model":            strings.Replace(valid, `"prompt":`, `"model":42,"prompt":`, 1),
		"пустой effort":             strings.Replace(valid, `"prompt":`, `"effort":"","prompt":`, 1),
		"effort с пробелом":         strings.Replace(valid, `"prompt":`, `"effort":"very high","prompt":`, 1),
		"неверный speed":            strings.Replace(valid, `"prompt":`, `"speed":"turbo","prompt":`, 1),
		"числовой speed":            strings.Replace(valid, `"prompt":`, `"speed":2,"prompt":`, 1),
		// Разные непарные суррогаты не должны превращаться в один ID через замену на U+FFFD.
		"искажённая ссылка": `{"id":"flow","steps":[{"id":"\ud800","type":"agent","prompt":"x","dependsOn":[]},{"id":"b","type":"agent","prompt":"x","dependsOn":["\ud801"]}]}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			w, err := Decode(strings.NewReader(input))
			if err == nil || !reflect.DeepEqual(w, Workflow{}) {
				t.Fatalf("ожидались ошибка и пустой результат: %+v, %v", w, err)
			}
		})
	}
}

// TestDecodeRejectsNullRuntimeSettings защищает разницу между отсутствующим
// полем и явным null. Отсутствие наследует Codex, а null является неверным
// значением и должен назвать конкретное поле до создания run.
func TestDecodeRejectsNullRuntimeSettings(t *testing.T) {
	cases := map[string]string{
		"workflow.model": strings.Replace(valid, `"id":"flow"`, `"id":"flow","model":null`, 1),
		"step.model":     strings.Replace(valid, `"prompt":`, `"model":null,"prompt":`, 1),
		"step.effort":    strings.Replace(valid, `"prompt":`, `"effort":null,"prompt":`, 1),
		"step.speed":     strings.Replace(valid, `"prompt":`, `"speed":null,"prompt":`, 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			field := name[strings.LastIndex(name, ".")+1:]
			w, err := Decode(strings.NewReader(input))
			if err == nil || !reflect.DeepEqual(w, Workflow{}) || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "null") {
				t.Fatalf("явный null для %s принят или потерял понятную диагностику: %+v, %v", name, w, err)
			}
		})
	}
}

// TestDecodeRejectsInvalidUnicode защищает ID и промпты от незаметной замены
// повреждённых байтов и непарных суррогатов на U+FFFD при чтении JSON.
func TestDecodeRejectsInvalidUnicode(t *testing.T) {
	for name, raw := range map[string]string{"UTF-8": "\xff", "старший суррогат": `\ud800`, "младший суррогат": `\udc00`} {
		for field, old := range map[string]string{"workflow.id": `"flow"`, "step.id": `"a"`, "prompt": `"  задача\n"`} {
			t.Run(name+"/"+field, func(t *testing.T) {
				input := strings.Replace(valid, old, `"`+raw+`"`, 1)
				w, err := Decode(strings.NewReader(input))
				if err == nil || !reflect.DeepEqual(w, Workflow{}) {
					t.Fatalf("повреждённый Unicode принят: %+v, %v", w, err)
				}
			})
		}
	}
}

// TestDecodePreservesUnicode отличает повреждённый ввод от допустимых символов:
// буквальный U+FFFD и корректная суррогатная пара не должны отклоняться.
func TestDecodePreservesUnicode(t *testing.T) {
	for _, raw := range []string{"� 😀", `\ufffd \ud83d\ude00`} {
		input := strings.Replace(valid, `"flow"`, `"`+raw+`"`, 1)
		input = strings.Replace(input, `"  задача\n"`, `"`+raw+`"`, 1)
		w, err := Decode(strings.NewReader(input))
		if err != nil || w.ID != "� 😀" || len(w.Steps) != 1 || w.Steps[0].Prompt != "� 😀" {
			t.Fatalf("допустимый Unicode изменён или отклонён: %+v, %v", w, err)
		}
	}
}

// TestValidateGraph проверяет независимые ветки, сборщик, некорректные ссылки и
// цикл в отдельной компоненте: наличие корректного корня не делает граф корректным.
func TestValidateGraph(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps map[string][]string
		bad  bool
	}{
		{"ветвление и сборщик", map[string][]string{"a": {}, "b": {"a"}, "c": {"a"}, "d": {"b", "c"}}, false},
		{"независимые", map[string][]string{"a": {}, "b": {}}, false},
		{"неизвестная ссылка", map[string][]string{"a": {"missing"}}, true},
		{"повтор ребра", map[string][]string{"a": {}, "b": {"a", "a"}}, true},
		{"самозависимость", map[string][]string{"a": {"a"}}, true},
		{"отдельный цикл", map[string][]string{"a": {}, "b": {"c"}, "c": {"b"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := Workflow{ID: "graph"}
			for id, deps := range tc.deps {
				w.Steps = append(w.Steps, Step{ID: id, Type: "agent", Prompt: "задача", DependsOn: deps})
			}
			if err := w.Validate(); (err != nil) != tc.bad {
				t.Fatalf("ошибка: %v; ожидалась ошибка: %v", err, tc.bad)
			}
		})
	}
	w, _ := Decode(strings.NewReader(valid))
	w.Steps = append(w.Steps, w.Steps[0])
	if err := w.Validate(); err == nil {
		t.Fatal("повторные id должны отклоняться")
	}
}

// TestLongChain защищает итеративный обход и отсутствие изменений порядка Steps.
func TestLongChain(t *testing.T) {
	w := Workflow{ID: "chain"}
	for i := 9999; i >= 0; i-- {
		deps := []string{}
		if i > 0 {
			deps = append(deps, fmt.Sprint(i-1))
		}
		w.Steps = append(w.Steps, Step{ID: fmt.Sprint(i), Type: "agent", Prompt: "задача", DependsOn: deps})
	}
	if err := w.Validate(); err != nil || w.Steps[0].ID != "9999" {
		t.Fatalf("цепочка изменена или отклонена: %v", err)
	}
}

// failingReader имитирует ошибку источника; она не должна выглядеть успешным EOF.
type failingReader struct{ err error }

// Read возвращает заданную ошибку без данных, не подменяя её концом потока.
func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// TestReadError проверяет ошибки как до объекта, так и при чтении его окончания.
func TestReadError(t *testing.T) {
	failure := errors.New("ошибка чтения")
	for _, prefix := range []string{"", valid} {
		_, err := Decode(io.MultiReader(strings.NewReader(prefix), failingReader{failure}))
		if !errors.Is(err, failure) {
			t.Fatalf("ошибка источника потеряна: %v", err)
		}
	}
}

// FuzzDecode проверяет отсутствие паник на произвольном вводе. Принятая схема
// обязана оставаться корректной и неизменной после повторного JSON-кодирования.
func FuzzDecode(f *testing.F) {
	for _, seed := range []string{valid, `null`, `{}`, valid + `{}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		w, err := Decode(bytes.NewReader(input))
		if err != nil {
			return
		}
		data, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Decode(bytes.NewReader(data))
		if err != nil || !reflect.DeepEqual(w, again) {
			t.Fatalf("повторная проверка изменила схему: %v", err)
		}
	})
}
