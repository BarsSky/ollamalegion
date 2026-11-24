// runner_test.go — разбор строк python-драйвера.
//
// ЗАЧЕМ БЕЗ ПРОЦЕССА. Формат «одна строка — одно событие» — единственный контракт
// между python и Go. Проверять его через настоящий python3 в тестах значит
// зависеть от установленного интерпретатора и от 16-гигабайтного файла; здесь
// проверяется ровно преобразование «строка → событие задачи», включая стадии
// reading/writing/validating, на которых держится прогресс normalize и convert.
package main

import (
	"strings"
	"testing"
)

func TestDriverLineEventParsesStages(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		kind    string
		stage   string
		phase   string
		percent float64
	}{
		{
			name:    "стадия из нового драйвера",
			line:    `{"event":"progress","stage":"reading","phase":"plan","percent":0}`,
			kind:    "progress",
			stage:   "reading",
			phase:   "plan",
			percent: 0,
		},
		{
			name:    "запись с процентами по байтам",
			line:    `{"event":"progress","stage":"writing","percent":42.5,"bytes_done":1024,"bytes_total":2048}`,
			kind:    "progress",
			stage:   "writing",
			phase:   "copy", // метка для WebUI выводится из стадии
			percent: 42.5,
		},
		{
			// Старые образы печатали только phase: стадия обязана вывестись из неё,
			// иначе после обновления Go-хоста прогресс старых драйверов «пропадёт».
			name:    "старый драйвер только с phase",
			line:    `{"event":"progress","phase":"verify","percent":99}`,
			kind:    "progress",
			stage:   "validating",
			phase:   "verify",
			percent: 99,
		},
		{
			name:    "сверка результата",
			line:    `{"event":"progress","stage":"validating","phase":"verify","percent":100}`,
			kind:    "progress",
			stage:   "validating",
			phase:   "verify",
			percent: 100,
		},
		{
			name: "лог",
			line: `{"event":"log","message":"пишу результат"}`,
			kind: "log",
		},
		{
			name: "не JSON",
			line: "Traceback (most recent call last):",
			kind: "log",
		},
		{
			name: "битый JSON",
			line: `{"event":"progress",`,
			kind: "log",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := driverLineEvent(c.line)
			if ev.Type != c.kind {
				t.Fatalf("тип события %q, ожидался %q (строка: %s)", ev.Type, c.kind, c.line)
			}
			if c.kind != "progress" {
				return
			}
			if ev.Progress.Stage != c.stage {
				t.Fatalf("стадия %q, ожидалась %q", ev.Progress.Stage, c.stage)
			}
			if ev.Progress.Phase != c.phase {
				t.Fatalf("метка %q, ожидалась %q", ev.Progress.Phase, c.phase)
			}
			if ev.Progress.Percent != c.percent {
				t.Fatalf("процент %v, ожидался %v", ev.Progress.Percent, c.percent)
			}
		})
	}
}

func TestDriverLineEventCarriesResultAndError(t *testing.T) {
	ev := driverLineEvent(`{"event":"result","duration_ms":1407,"src_bytes":10,"dst_bytes":20,` +
		`"facts":{"arch":"inkling2"},"changes":["block_count: 48 -> 24"],"verify":{"ok":true}}`)
	if ev.Type != "result" || ev.Result == nil {
		t.Fatalf("итог не разобран: %+v", ev)
	}
	if ev.Result.DurationMS != 1407 || ev.Result.SrcBytes != 10 || ev.Result.DstBytes != 20 {
		t.Fatalf("числа итога потеряны: %+v", ev.Result)
	}
	if len(ev.Result.Changes) != 1 || ev.Result.Facts["arch"] != "inkling2" {
		t.Fatalf("детали итога потеряны: %+v", ev.Result)
	}

	ev = driverLineEvent(`{"event":"error","code":"model_not_patchable","message":"не inkling2","detail":"x"}`)
	if ev.Type != "error" || ev.Error == nil {
		t.Fatalf("ошибка не разобрана: %+v", ev)
	}
	if ev.Error.Code != "model_not_patchable" || ev.Error.Detail != "x" {
		t.Fatalf("поля ошибки потеряны: %+v", ev.Error)
	}
}

// TestConsumeStdoutStreamsStageSequence — поток целиком: три стадии, затем итог.
func TestConsumeStdoutStreamsStageSequence(t *testing.T) {
	stream := strings.Join([]string{
		`{"event":"progress","stage":"reading","phase":"plan","percent":0}`,
		"служебный шум python",
		`{"event":"progress","stage":"writing","phase":"copy","percent":10,"bytes_done":1,"bytes_total":10}`,
		`{"event":"progress","stage":"writing","phase":"copy","percent":99,"bytes_done":9,"bytes_total":10}`,
		`{"event":"progress","stage":"validating","phase":"verify","percent":100}`,
		`{"event":"result","duration_ms":1,"src_bytes":10,"dst_bytes":10}`,
	}, "\n")

	var stages []string
	var gotLog bool
	var gotResult bool
	r := &pythonRunner{}
	r.consumeStdout(strings.NewReader(stream), func(ev runEvent) {
		switch ev.Type {
		case "progress":
			stages = append(stages, ev.Progress.Stage)
		case "log":
			gotLog = true
		case "result":
			gotResult = true
		}
	})
	want := []string{"reading", "writing", "writing", "validating"}
	if strings.Join(stages, ",") != strings.Join(want, ",") {
		t.Fatalf("стадии %v, ожидались %v", stages, want)
	}
	if !gotLog {
		t.Fatal("строка не-JSON не попала в лог: оператор потеряет причину отказа python")
	}
	if !gotResult {
		t.Fatal("итог из потока не дошёл до менеджера")
	}
}
