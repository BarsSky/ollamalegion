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
	//
	// 2026-11-14 (этап 6): цикл SSE /v1 стал ОДНИМ и живёт в
	// stream_openai_sse.go — там же создаётся сторож, а не в одном из
	// транспортов. Записи proxy_request.go и proxy_request_hijack.go убраны: они
	// больше не читают стрим (тела переехали), и реестр не должен врать.
	"stream_openai_sse.go": {covered: true},
	// R630: транспорт разложен на пять файлов; сторож тишины создаётся в слое
	// чтения апстрима (startUpstreamReaders), а не в самом транспортном файле.
	"llamacpp_stream_upstream.go": {covered: true},

	"proxy_request_openai_auto_stream.go": {
		why: "авто-стрим для клиентов, попросивших stream:false (не агентский путь)",
	},
	"proxy_request_openai_auto_stream_limits.go": {
		why: "то же, версия R258",
	},
	"llamacpp_transport_nonstream.go": {
		why: "не-стриминговый сбор ответа: есть собственный общий таймаут запроса",
	},
	"preflight_stream_dialog.go": {
		why: "короткий служебный диалог preflight, не ответ клиенту",
	},
	"anthropic_api.go": {
		why: "адаптер Anthropic — пробел R298, сторож не подключён",
	},
	"responses_api.go": {
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
		// Обнаружение — по ОДНОМУ файлу: склейка слоя сделала бы «читающим стрим»
		// каждый его файл (R630).
		src := stallWatchFileSourceR298(t, name)
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
		// «covered» проверяется по источнику проверки: для файлов транспортного слоя —
		// весь слой (R630: создание сторожа и его остановка оказались в разных файлах
		// одного слоя), для остальных — ровно один файл.
		src := stallWatchCoverageSourceR298(t, name)
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

// stallWatchCoverageSourceR298 — текст, по которому проверяется «covered».
//
// R630: сторож тишины теперь живёт в ДВУХ файлах одного слоя — создаётся и
// запускается в llamacpp_stream_upstream.go (startUpstreamReaders), а
// останавливается `defer st.stallWatch.Stop()` в llamacpp_transport.go, там же,
// где начинается стрим. Проверка «в файле есть все четыре маркера» после такого
// переноса краснела бы на корректном коде, поэтому для файлов транспортного слоя
// читается весь слой (см. llamacpp_sources_r630_test.go). Требования не
// ослаблены: все маркеры обязаны быть в слое, просто не в одной строке одного файла.
func stallWatchCoverageSourceR298(t *testing.T, name string) string {
	t.Helper()
	for _, layerFile := range llamaCppTransportLayerFilesR630 {
		if layerFile == name {
			return readLlamaCppSourcesR630(t)
		}
	}
	return stallWatchFileSourceR298(t, name)
}

// stallWatchFileSourceR298 — текст РОВНО одного файла (без склейки слоя).
func stallWatchFileSourceR298(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatalf("ReadFile %s: %v", name, err)
	}
	return string(data)
}
