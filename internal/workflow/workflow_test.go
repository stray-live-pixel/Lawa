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
		"null шага":            `{"id":"flow","steps":[null]}`,
		"пустой id шага":       strings.Replace(valid, `"id":"a"`, `"id":" "`, 1),
		"нет id шага":          strings.Replace(valid, `"id":"a",`, "", 1),
		"нет type":             strings.Replace(valid, `"type":"agent",`, "", 1),
		"неверный type":        strings.Replace(valid, `"agent"`, `"shell"`, 1),
		"нет prompt":           strings.Replace(valid, `"prompt":"  задача\n",`, "", 1),
		"пустой prompt":        strings.Replace(valid, `"  задача\n"`, `" \n"`, 1),
		"null prompt":          strings.Replace(valid, `"  задача\n"`, `null`, 1),
		"нет dependsOn":        strings.Replace(valid, `,"dependsOn":[]`, "", 1),
		"null dependsOn":       strings.Replace(valid, `"dependsOn":[]`, `"dependsOn":null`, 1),
		"тип dependsOn":        strings.Replace(valid, `"dependsOn":[]`, `"dependsOn":"a"`, 1),
		"тип ссылки":           strings.Replace(valid, `"dependsOn":[]`, `"dependsOn":[42]`, 1),
		"неизвестное поле":     strings.Replace(valid, `"id":"flow"`, `"other":1,"id":"flow"`, 1),
		"регистр поля":         strings.Replace(valid, `"dependsOn"`, `"DependsOn"`, 1),
		"повтор поля workflow": strings.Replace(valid, `"id":"flow"`, `"id":"flow","id":"other"`, 1),
		"повтор поля шага":     strings.Replace(valid, `"id":"a"`, `"id":"a","\u0069d":"b"`, 1),
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
