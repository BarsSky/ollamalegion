// backend_rejections_file_absent_r493_test.go — R493 (долги 11a/11b):
// «почему в очереди» обязано отличать УСТРАНИМУЮ причину от неустранимой.
//
// ЧТО БЫЛО. Когда модель не загружена, причина всегда называлась
// `model_not_loaded` — независимо от того, можно ли её вообще загрузить на этом
// узле. Каталог моделей на диске (R91/R93) ответ на это знал, но в why_queued не
// попадал: оператор видел «подожди, загрузится» на узле, где файла нет.
// Документация при этом УЖЕ обещала `file_absent: <узел>`
// (docs/architecture/availability-first-r493.ru.md §4) — обещание и код разошлись.
//
// ТРИ СОСТОЯНИЯ КАТАЛОГА — ТРИ РАЗНЫХ ОТВЕТА (и тест держит каждое):
//   - файла нет (снимок прочитан)          → file_absent;
//   - каталог недоступен (R93 dirError)    → catalog_unavailable (не «файла нет»!);
//   - снимка нет (узел не опрашивался)     → model_not_loaded (не знаем — не врём).
package balancer

import (
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// catalogProxyR493 — прокси с каталогом моделей (availProxyR493 его не заводит).
func catalogProxyR493(t *testing.T, slots map[string]int) *Proxy {
	t.Helper()
	p := availProxyR493(t, slots)
	if p.modelCatalog == nil { p.modelCatalog = newModelCatalog() } else { p.modelCatalog.reset() }
	return p
}

// TestR493_RejectionNamesFileAbsent — файла нет на диске узла: причина обязана
// называться, и в ней обязано быть видно, что каталог ЧИТАЛСЯ (число файлов), а
// не «пусто, потому что сломалось».
func TestR493_RejectionNamesFileAbsent(t *testing.T) {
	p := catalogProxyR493(t, map[string]int{"has-file": 1, "no-file": 1})
	p.modelCatalog.store("has-file", []string{"m12b.gguf", "other.gguf"}, "", time.Now())
	p.modelCatalog.store("no-file", []string{"other.gguf", "third.gguf"}, "", time.Now())
	setMetricsR493(p, "has-file", nil, 20490) // VRAM хватает обоим
	setMetricsR493(p, "no-file", nil, 20490)

	list := p.backendRejectionsR460("m12b", types.BackendTypeLlamaCpp)

	reason, detail, found := reasonForR493(list, "no-file")
	if !found {
		t.Fatalf("узел без файла модели вообще не попал в список причин: %+v", list)
	}
	if reason != rejectFileAbsentR460 {
		t.Fatalf("причина %q, ожидалась %q: загрузкой этот отказ не устранить, "+
			"значит и называться он не должен как «не загружена»", reason, rejectFileAbsentR460)
	}
	for _, want := range []string{"нет на диске", "файлов в каталоге: 2"} {
		if !strings.Contains(detail, want) {
			t.Errorf("в причине нет %q: %q — оператор не отличит «сломанный каталог» от «файла нет»", want, detail)
		}
	}
	t.Logf("причина названа: %s = %s", reason, detail)

	// Контроль: на узле, где файл ЕСТЬ, загрузка устранима — причина прежняя.
	reason, detail, found = reasonForR493(list, "has-file")
	if !found {
		t.Fatalf("узел с файлом пропал из списка причин: %+v", list)
	}
	if reason != rejectModelAbsentR460 {
		t.Fatalf("при наличии файла причина %q, ожидалась %q", reason, rejectModelAbsentR460)
	}
	t.Logf("контроль: %s = %s", reason, detail)
}

// TestR493_FileAbsentIsNotClaimedForAliasedName — защита от ЛОЖНОГО обвинения:
// в каталоге имя файла с расширением, клиент просит без него. Это одна модель.
func TestR493_FileAbsentIsNotClaimedForAliasedName(t *testing.T) {
	p := catalogProxyR493(t, map[string]int{"node": 1})
	p.modelCatalog.store("node", []string{"gemma-4-E4B-it-Q4_K_M.gguf"}, "", time.Now())
	setMetricsR493(p, "node", nil, 20490)

	// Три формы одного имени: как у воркера, как у клиента, полный путь в логах.
	for _, name := range []string{
		"gemma-4-E4B-it-Q4_K_M.gguf",
		"gemma-4-E4B-it-Q4_K_M",
		"/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
	} {
		list := p.backendRejectionsR460(name, types.BackendTypeLlamaCpp)
		reason, detail, found := reasonForR493(list, "node")
		if !found {
			t.Fatalf("%q: узел пропал из причин", name)
		}
		if reason == rejectFileAbsentR460 {
			t.Fatalf("%q: файл ЛЕЖИТ на узле, а причина %q (%s) — оператор пойдёт копировать "+
				"то, что уже есть", name, reason, detail)
		}
	}
}

// TestR493_RejectionCatalogUnavailableIsNotFileAbsent — R93: пустой список при
// dirError — это «каталог узла отвалился», а не «моделей нет». Называть здесь
// file_absent значит соврать: файл мог быть.
func TestR493_RejectionCatalogUnavailableIsNotFileAbsent(t *testing.T) {
	p := catalogProxyR493(t, map[string]int{"broken": 1})
	p.modelCatalog.store("broken", nil, "open /app/models: input/output error", time.Now())
	setMetricsR493(p, "broken", nil, 20490)

	list := p.backendRejectionsR460("m12b", types.BackendTypeLlamaCpp)
	reason, detail, found := reasonForR493(list, "broken")
	if !found {
		t.Fatalf("узел с недоступным каталогом не попал в причины: %+v", list)
	}
	if reason != rejectCatalogUnavailableR460 {
		t.Fatalf("причина %q, ожидалась %q", reason, rejectCatalogUnavailableR460)
	}
	if !strings.Contains(detail, "input/output error") {
		t.Errorf("в причине нет ошибки воркера: %q — без неё неотличимо от «файла нет»", detail)
	}
}

// TestR493_UnknownCatalogKeepsModelNotLoaded — снимка нет: «не знаем» не равно
// «файла нет». Утверждать отсутствие нельзя, и тест это держит.
func TestR493_UnknownCatalogKeepsModelNotLoaded(t *testing.T) {
	p := catalogProxyR493(t, map[string]int{"unpolled": 1})
	setMetricsR493(p, "unpolled", nil, 20490) // каталог узла НЕ опрашивался

	list := p.backendRejectionsR460("m12b", types.BackendTypeLlamaCpp)
	reason, _, found := reasonForR493(list, "unpolled")
	if !found {
		t.Fatalf("узел не попал в причины: %+v", list)
	}
	if reason != rejectModelAbsentR460 {
		t.Fatalf("без снимка каталога причина %q, ожидалась %q: «не опрашивали» — не «файла нет»",
			reason, rejectModelAbsentR460)
	}
}

// TestR493_FileAbsentWinsOverVram — порядок причин: если файла нет, VRAM уже
// неважна (копировать некуда), и оператору незачем читать про гибибайты.
func TestR493_FileAbsentWinsOverVram(t *testing.T) {
	restore := setModelSizeBytesForTest(func(name string) int64 {
		if name == "m12b" {
			return int64(12) << 30
		}
		return 0
	})
	defer restore()

	p := catalogProxyR493(t, map[string]int{"tiny": 1})
	p.modelCatalog.store("tiny", []string{"other.gguf"}, "", time.Now())
	setMetricsR493(p, "tiny", nil, 1000) // 1 ГБ свободно: 12 ГиБ не влезет

	list := p.backendRejectionsR460("m12b", types.BackendTypeLlamaCpp)
	reason, detail, found := reasonForR493(list, "tiny")
	if !found {
		t.Fatalf("узел не попал в причины: %+v", list)
	}
	if reason != rejectFileAbsentR460 {
		t.Fatalf("причина %q, ожидалась %q: файла нет — про VRAM говорить нечего (%s)",
			reason, rejectFileAbsentR460, detail)
	}
}

// TestR493b_LoadStageDoesNotPickNodeWithoutFile — долг 11b, механизм ЦЕЛИКОМ.
//
// Этап 3b (R493, «свободный узел под загрузку вместо очереди к занятому»)
// выбирал узел по VRAM и НЕ спрашивал каталог, хотя его же комментарий обещает
// «не пытается грузить модель на узле, где её нет». Живая последовательность:
// прогретая копия занята, рядом свободный узел с 8 ГБ без файла → загрузка там
// падает `model_not_found`, клиент получает 503, хотя достаточно было дождаться
// слота готовой копии.
func TestR493b_LoadStageDoesNotPickNodeWithoutFile(t *testing.T) {
	restore := setModelSizeBytesForTest(func(name string) int64 {
		if name == "m12b" {
			return int64(3) << 30 // влезает и в 8-гигабайтный узел
		}
		return 0
	})
	defer restore()

	p := catalogProxyR493(t, map[string]int{"master": 1, "other": 1})
	setMetricsR493(p, "master", []string{"m12b"}, 2471)
	p.backends["master"].ActiveReqs = 1 // прогретая копия занята
	setMetricsR493(p, "other", nil, 6000)
	p.modelCatalog.store("master", []string{"m12b.gguf"}, "", time.Now())
	p.modelCatalog.store("other", []string{"unrelated.gguf"}, "", time.Now())
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if got := p.pickLoadableFreeBackendR493("m12b", types1); got != "" {
		t.Fatalf("выбран %q — узел без файла модели: загрузка там даст 503 model_not_found, "+
			"а рядом ждёт слота готовая копия", got)
	}

	// Контроль 1: файл появился на свободном узле — он снова кандидат.
	p.modelCatalog.store("other", []string{"m12b.gguf"}, "", time.Now())
	if got := p.pickLoadableFreeBackendR493("m12b", types1); got != "other" {
		t.Fatalf("узел с файлом обязан выбираться под загрузку, got %q", got)
	}

	// Контроль 2: каталог НЕ опрошен — узел не исключаем (иначе при выключенном
	// поллере каталога из выбора выпал бы весь кластер).
	if p.modelCatalog == nil { p.modelCatalog = newModelCatalog() } else { p.modelCatalog.reset() }
	if got := p.pickLoadableFreeBackendR493("m12b", types1); got != "other" {
		t.Fatalf("без снимка каталога узел нельзя исключать, got %q", got)
	}

	// Контроль 3: каталог узла отвалился (dirError) — тоже не исключаем: файл мог
	// быть, а «каталог недоступен» не равно «файла нет».
	p.modelCatalog.store("other", nil, "open /app/models: input/output error", time.Now())
	if got := p.pickLoadableFreeBackendR493("m12b", types1); got != "other" {
		t.Fatalf("по недоступному каталогу узел исключать нельзя, got %q", got)
	}
}

// TestR493b_CatalogNeverExcludesCandidateSilently — долг 11b.
//
// Жалоба была: «узел молча выпадает из выбора по каталогу». Факт (проверено на
// стенде 2026-11-14 и здесь): expandCandidates каталог НЕ читает, поэтому узел с
// файлом на диске не выпадает из P3 ни при каком состоянии каталога. А когда
// каталог решает (путь загрузки findLeastLoadedBackendWithModelOnDisk), отсутствие
// файла — причина, и теперь она называет себя. Тест фиксирует оба утверждения,
// чтобы «оптимизация» отбора не сделала исключение молчаливым.
func TestR493b_CatalogNeverExcludesCandidateSilently(t *testing.T) {
	p := catalogProxyR493(t, map[string]int{"loaded": 1, "free-no-entry": 1})
	p.modelCatalog.store("loaded", []string{"m12b.gguf"}, "", time.Now())
	p.modelCatalog.store("free-no-entry", []string{"unrelated.gguf"}, "", time.Now())
	setMetricsR493(p, "loaded", []string{"m12b"}, 800) // занят: слот 1/1
	setMetricsR493(p, "free-no-entry", nil, 20490)     // свободен, файла m12b нет
	p.backends["loaded"].ActiveReqs = 1

	var freeGroup []string
	for _, g := range p.expandCandidates("m12b", nil) {
		if g.Priority == 3 {
			freeGroup = g.BackendIDs
		}
	}
	if len(freeGroup) != 1 || freeGroup[0] != "free-no-entry" {
		t.Fatalf("P3 = %v, ожидался free-no-entry: каталог НЕ должен выкидывать узел "+
			"из отбора (11b) — иначе исключение станет молчаливым", freeGroup)
	}

	// И причина у этого узла видна оператору — не пустая строка.
	list := p.backendRejectionsR460("m12b", types.BackendTypeLlamaCpp)
	reason, detail, found := reasonForR493(list, "free-no-entry")
	if !found {
		t.Fatalf("узел в P3, но в причинах его нет: оператор не узнает, почему заявка ждёт")
	}
	if reason != rejectFileAbsentR460 {
		t.Fatalf("причина %q (%s), ожидалась %q", reason, detail, rejectFileAbsentR460)
	}
}

// TestR493b_ColdStartPicksNodeWithFileOnDisk — 11b-хвост: при ХОЛОДНОМ старте
// (модель не загружена нигде) выбор обязан учитывать, у кого файл на диске.
//
// ЖИВОЙ ДЕФЕКТ (стенд, образ r261114-2356, после пересоздания воркера модель
// выгрузилась): запрос /api/chat выбрал cyborg — у него 8 ГБ свободной VRAM и
// лучший score, — а файла модели там нет. Авто-загрузка провалилась, клиент
// получил 503 «auto-load failed», хотя на мастере файл лежит и он свободен.
// Этап 3b молчал (он работает только когда прогретая копия занята), а этап 4
// (score по ресурсам) модели не знает вовсе.
func TestR493b_ColdStartPicksNodeWithFileOnDisk(t *testing.T) {
	p := catalogProxyR493(t, map[string]int{"master": 1, "other": 1})
	// Файл есть только у мастера; модель НИГДЕ не загружена (холодный старт).
	p.modelCatalog.store("master", []string{"m12b.gguf"}, "", time.Now())
	p.modelCatalog.store("other", []string{"unrelated.gguf"}, "", time.Now())
	setMetricsR493(p, "master", nil, 3000)
	setMetricsR493(p, "other", nil, 20000) // «лучший по ресурсам» — без файла

	if got := p.selectBackend("m12b", types.BackendTypeLlamaCpp); got != "master" {
		t.Fatalf("выбран %q, а файл модели есть только у master: загрузка там провалится "+
			"и клиент получит 503 (живой дефект 2026-11-14)", got)
	}

	// Контроль: каталог не опрошен — выбор ОБЯЗАН состояться, «не знаем» не равно
	// «файла нет». Какой именно узел выберется (master или other) — дело прежней
	// логики (P3/score); проверяем, что мы не ввели отказ там, где данных нет.
	if p.modelCatalog == nil { p.modelCatalog = newModelCatalog() } else { p.modelCatalog.reset() }
	if got := p.selectBackend("m12b", types.BackendTypeLlamaCpp); got != "master" && got != "other" {
		t.Fatalf("без снимка каталога выбор не должен отказывать, получено %q", got)
	}
}
