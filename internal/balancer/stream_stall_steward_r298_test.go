package balancer

// stream_stall_steward_r298_test.go — R298 (2026-10-29): СТЮАРД «ни один
// стриминговый путь не обходится без сторожа тишины».
//
// ЗАЧЕМ. В этой серии дефект «новый путь обошёл общий механизм» повторялся
// пять раз. Конкретно здесь он и случился: R297 завёл сторож зависшего стрима,
// но только в hijack-пути (/v1/chat/completions). Путь /api/chat (NDJSON, им
// ходит Cline через Ollama-протокол) сторож не получил — и на нём поток висел
// бесконечно, ровно как в жалобе оператора.
//
// Юнит-тесты такое не ловят: каждый путь по отдельности выглядит корректным.
// Поэтому здесь сканируется ИСХОДНИК пакета: любой файл, который блокирующе
// читает стрим построчно, обязан быть в реестре ниже — либо как «сторож есть»,
// либо как явно зафиксированный пробел с причиной. Появился новый файл —
// тест падает и требует решения, а не молчаливого пробела.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// stallPathStatus — что известно про стриминговый путь в файле.
type stallPathStatus struct {
	// covered=true: сторож тишины подключён (newStreamStallWatch).
	covered bool
	// why — причина, по которой сторож не нужен / пока не подключён. Пустая
	// строка для covered=true.
	why string
}

// stallPathRegistryR298 — КАЖДЫЙ файл пакета, который блокирующе читает стрим.
//
// Список — это решение, а не украшение: сюда попадают только осознанно.
var stallPathRegistryR298 = map[string]stallPathStatus{
	// Сторож подключён.
	"proxy_request_hijack.go": {covered: true},
	"llamacpp_transport.go":   {covered: true},

	// Пока НЕ подключён — зафиксированные пробелы R298. Каждый требует
	// отдельного решения, и тест не даст про них забыть.
	"proxy_request.go": {
		why: "не-hijack путь /v1/chat/completions (буферизующий). Пробел R298: " +
			"сторож не подключён; при LB_STREAMING_NEVER_TIMEOUT=1 поток может висеть",
	},
	"proxy_request_openai_auto_stream.go": {
		why: "авто-стрим для клиентов, попросивших stream:false (не агентский путь)",
	},
	"proxy_request_openai_auto_stream_r258.go": {
		why: "то же, версия R258",
	},
	"llamacpp_transport_nonstream.go": {
		why: "не-стриминговый сбор ответа: есть собственный общий таймаут запроса",
	},
	"preflight_stream_dialog.go": {
		why: "короткий служебный диалог preflight, не ответ клиенту",
	},
	"anthropic_r247.go": {
		why: "адаптер Anthropic — пробел R298, сторож не подключён",
	},
	"responses_r247.go": {
		why: "адаптер /v1/responses — пробел R298, сторож не подключён",
	},
	"virtual_router.go": {
		why: "внутренний виртуальный роутер — пробел R298, сторож не подключён",
	},
}

// blockingStreamReadPatterns — как выглядит блокирующее построчное чтение стрима.
var blockingStreamReadPatterns = []string{
	"for scanner.Scan() {",
	".ReadBytes('\\n')",
	"reader.ReadString('\\n')",
}

// TestR298_EveryStreamingPathIsRegistered — стюард.
func TestR298_EveryStreamingPathIsRegistered(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var unregistered []string
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Тестовые файлы не читают стрим клиенту.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		src := string(data)
		hasRead := false
		for _, pat := range blockingStreamReadPatterns {
			if strings.Contains(src, pat) {
				hasRead = true
				break
			}
		}
		if !hasRead {
			continue
		}
		seen[name] = true
		if _, ok := stallPathRegistryR298[name]; !ok {
			unregistered = append(unregistered, name)
		}
	}

	sort.Strings(unregistered)
	if len(unregistered) > 0 {
		t.Fatalf("новые стриминговые пути без решения про сторож тишины (R298): %v\n"+
			"Подключите newStreamStallWatch (как в llamacpp_transport.go) или внесите файл "+
			"в stallPathRegistryR298 с причиной — молчаливый пробел здесь уже приводил к "+
			"бесконечному ожиданию у клиента.", unregistered)
	}

	// Обратная проверка: реестр не должен гнить. Если файл из реестра больше не
	// читает стрим (переписали), запись пора убрать — иначе реестр врёт.
	for name := range stallPathRegistryR298 {
		if !seen[name] {
			t.Errorf("%s записан в stallPathRegistryR298, но блокирующего чтения стрима там больше нет — уберите запись", name)
		}
	}
}

// TestR298_CoveredPathsActuallyUseTheSharedWatch — «covered» не на слово:
// в этих файлах обязан быть вызов общего сторожа. Именно этой проверки не
// хватало R297: сторож был, а путь его не вызывал.
func TestR298_CoveredPathsActuallyUseTheSharedWatch(t *testing.T) {
	for name, st := range stallPathRegistryR298 {
		if !st.covered {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		src := string(data)
		if !strings.Contains(src, "newStreamStallWatch(") {
			t.Errorf("%s помечен как covered, но не создаёт сторож через newStreamStallWatch", name)
		}
		if !strings.Contains(src, "stallWatch.Start()") {
			t.Errorf("%s: сторож создан, но не запущен (stallWatch.Start отсутствует)", name)
		}
		if !strings.Contains(src, "stallWatch.Stop()") {
			t.Errorf("%s: сторож не останавливается (stallWatch.Stop отсутствует) — утечка горутины на каждый стрим", name)
		}
		if !strings.Contains(src, "stallWatch.Touch()") {
			t.Errorf("%s: данные не сбрасывают отсчёт тишины (stallWatch.Touch отсутствует) — сторож сработает на живом стриме", name)
		}
	}
}
