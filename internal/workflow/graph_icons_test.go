package workflow

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

// Визуальные метаданные проходят строгий decode и round-trip снимка; ошибочная
// иконка отклоняется до запуска, а отсутствие полей не меняет старый контракт.
func TestGraphPresentation(t *testing.T) {
	for _, version := range []string{`"dependsOn":[]`, `"after":[]`} {
		prefix := ""
		if version == `"after":[]` {
			prefix = `"version":2,"start":["a"],`
		}
		source := `{` + prefix + `"id":"test","steps":[{"id":"a","type":"agent","prompt":"test",` + version + `,"icon":"Code"}]}`
		var w Workflow
		if err := json.Unmarshal([]byte(source), &w); err != nil {
			t.Fatal(err)
		}
		if err := w.Validate(); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"icon":"Code"`) {
			t.Fatal(string(data))
		}
		for _, invalid := range []string{`"UnknownIcon"`, `"../Code"`, `""`, `null`} {
			var broken Workflow
			err := json.Unmarshal([]byte(strings.Replace(source, `"Code"`, invalid, 1)), &broken)
			if err == nil {
				err = broken.Validate()
			}
			if err == nil {
				t.Fatalf("icon=%s принята", invalid)
			}
		}
	}
	source := `{"version":2,"id":"labels","start":["a"],"steps":[{"id":"a","type":"agent","prompt":"test","after":[],"decisions":{"custom":{"finish":"succeeded","label":"Можно публиковать"}}}]}`
	var w Workflow
	if err := json.Unmarshal([]byte(source), &w); err != nil {
		t.Fatal(err)
	}
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
	if *w.Steps[0].Decisions["custom"].Label != "Можно публиковать" {
		t.Fatal("потеряна label")
	}
	for _, invalid := range []string{`" "`, `null`, `12`} {
		var broken Workflow
		err := json.Unmarshal([]byte(strings.Replace(source, `"Можно публиковать"`, invalid, 1)), &broken)
		if err == nil {
			err = broken.Validate()
		}
		if err == nil {
			t.Fatalf("label=%s принята", invalid)
		}
	}
}
