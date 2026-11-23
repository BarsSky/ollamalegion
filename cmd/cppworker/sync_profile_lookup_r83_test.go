//go:build llama_stub

// sync_profile_lookup_r83_test.go — R83 §3.4 (2026-09-26): живой резолв профиля.
//
// ИСТОРИЯ. R83 §3.4 закрывал живой дефект: гейт n_ctx считал KV типом q4_0, а
// llama.cpp получал профильный q8_0, потому что профиль не находился по имени,
// которым зовёт клиент ("qwen3.8:latest"). Тогда (и это была ПЕРВАЯ правка)
// резолвер стал сравнивать нормализованные ключи по ПРЕФИКСУ и брать самый
// длинный совпавший ключ.
//
// D1 (2026-11-25) заменил ЭТО правило на лестницу без подстрок/префиксов
// (cmd/cppworker/profile_key.go): префиксный поиск подставлял ЧУЖОЙ профиль —
// «самый длинный ключ» перебивал точное совпадение, и имя `M.gguf` резолвилось
// в соседний `M-mini` (32768 вместо 131072). На стенде профили-префиксы живут
// рядом (Qwen3.8-27B / Qwen3.8-27B-UD-Q4_K_M, gemma-4 / gemma-4-E4B-it-Q4_K_M).
//
// ЧТО СТАЛО С ПЕРВОНАЧАЛЬНЫМ ДЕФЕКТОМ R83. Он остаётся закрытым, но не
// угадыванием в резолвере, а правилом ВЛАДЕЛЬЦА ФАЙЛА (R154,
// profileLookupNameForPath): загрузка/перезагрузка ищет профиль по имени файла
// без расширения — точное совпадение с ключом профиля. Это проверяет
// profile_owner_r154_test.go.
//
// Поэтому тесты ниже переписаны под контракт D1: «профиль находится точно» и
// «чужой профиль не подставляется». Угадывание по тегу больше не проверяется —
// оно и было источником подмены.
package main

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newSeededProfileSyncerR83 — syncer с профилями из реального ответа балансера
// (/api/v1/cppworker/model-profiles), без сети.
func newSeededProfileSyncerR83() *profileSyncerT {
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{}}
	ps.cache["Qwen3.8-27B"] = types.LlamaCppModelProfile{
		ContextLength: 32768,
		BatchSize:     512,
		NumGPULayers:  -2,
		KVCacheType:   "q8_0",
	}
	ps.cache["Qwen3.6-35B-A3B-UD-Q4_K_M"] = types.LlamaCppModelProfile{
		ContextLength: 65536,
		KVCacheType:   "q4_0",
	}
	return ps
}

// TestR83_ProfileLookup_TaggedNameResolvesViaFileOwner — имя клиента с
// ollama-тегом резолвится в профиль НЕ угадыванием в резолвере, а именем
// владельца файла (R154) — и тогда это ТОЧНОЕ совпадение ключа.
func TestR83_ProfileLookup_TaggedNameResolvesViaFileOwner(t *testing.T) {
	ps := newSeededProfileSyncerR83()
	// Что делает загрузчик: сначала имя запроса → путь файла → владелец файла.
	owner := profileLookupNameForPath("qwen3.8:latest", "/app/models/Qwen3.8-27B.gguf")
	if owner != "Qwen3.8-27B" {
		t.Fatalf("владелец файла = %q, want Qwen3.8-27B (R154)", owner)
	}
	prof := ps.applyProfileOnLoad(owner)
	if prof == nil {
		t.Fatalf("профиль не найден для владельца файла %q: pull-sync профилей "+
			"не применяется (в кэше: Qwen3.8-27B, Qwen3.6-35B-A3B-UD-Q4_K_M)", owner)
	}
	if prof.KVCacheType != "q8_0" {
		t.Errorf("KVCacheType = %q, want q8_0 (профиль Qwen3.8-27B)", prof.KVCacheType)
	}
	// Голый тег в резолвере профиля НЕ ищется: угадывание по префиксу — это и
	// был механизм подмены чужого профиля (D1).
	if prof := ps.applyProfileOnLoad("qwen3.8:latest"); prof != nil {
		t.Errorf("для тега без пути подставлен профиль (ctx=%d): правильный ответ — "+
			"«профиля нет», а не сосед по префиксу", prof.ContextLength)
	}
}

// TestR83_ProfileLookup_NoVersionCollision — живой дефект, найденный на стенде:
// профиль "Qwen3-8B-Instruct-2507" не должен применяться к модели Qwen3.8-27B
// (версия Qwen3.8 и имя модели Qwen3-8B различаются).
func TestR83_ProfileLookup_NoVersionCollision(t *testing.T) {
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{}}
	// Профиль без kvCacheType — именно он «перебивал» нужный на живом стенде.
	ps.cache["Qwen3-8B-Instruct-2507"] = types.LlamaCppModelProfile{
		ContextLength: 32768,
		NumGPULayers:  -1,
	}
	ps.cache["Qwen3.8-27B-UD-Q4_K_M"] = types.LlamaCppModelProfile{
		ContextLength: 131072,
		NumGPULayers:  -2,
		KVCacheType:   "q8_0",
	}

	for i := 0; i < 50; i++ { // порядок обхода map случаен
		prof := ps.applyProfileOnLoad("Qwen3.8-27B-UD-Q4_K_M")
		if prof == nil {
			t.Fatal("профиль не найден")
		}
		if prof.KVCacheType != "q8_0" {
			t.Fatalf("итерация %d: выбран профиль Qwen3-8B (kv=%q gpu=%d) вместо "+
				"Qwen3.8-27B-UD-Q4_K_M (kv=q8_0 gpu=-2)", i, prof.KVCacheType, prof.NumGPULayers)
		}
		if prof.NumGPULayers != -2 {
			t.Fatalf("итерация %d: NumGPULayers = %d, want -2 (профиль Qwen3.8-27B-UD-Q4_K_M)",
				i, prof.NumGPULayers)
		}
		// Ключ без учёта регистра тоже не должен «съезжать» на Qwen3-8B.
		profLower := ps.applyProfileOnLoad("qwen3.8-27b-ud-q4_k_m")
		if profLower == nil || profLower.KVCacheType != "q8_0" {
			t.Fatalf("итерация %d: регистр имени сломал резолв (%+v)", i, profLower)
		}
	}
}

// TestR83_ProfileLookup_ExactAndCanonical — канонические имена тоже находятся
// (загрузка от балансера зовёт модель по имени файла).
func TestR83_ProfileLookup_ExactAndCanonical(t *testing.T) {
	ps := newSeededProfileSyncerR83()
	for _, name := range []string{
		"Qwen3.8-27B",
		"Qwen3.8-27B.gguf",
		"/app/models/Qwen3.8-27B.gguf",
	} {
		prof := ps.applyProfileOnLoad(name)
		if prof == nil {
			t.Errorf("профиль не найден для %q", name)
			continue
		}
		if prof.KVCacheType != "q8_0" {
			t.Errorf("%q: KVCacheType = %q, want q8_0", name, prof.KVCacheType)
		}
	}
}

// TestR83_Gate_UsesProfileKVType — сквозная проверка связки: если профиль
// находится, гейт обязан считать KV типом профиля (q8_0), а не дефолтом.
func TestR83_Gate_UsesProfileKVType(t *testing.T) {
	oldSyncer := profileSyncer
	defer func() { profileSyncer = oldSyncer }()
	profileSyncer = newSeededProfileSyncerR83()

	if got := effectiveKVCacheTypeForLoad("Qwen3.8-27B", ""); got != "q8_0" {
		t.Errorf("гейт считает KV типом %q, want q8_0 из профиля (R83 §3.4)", got)
	}
	// Явный параметр запроса по-прежнему главнее профиля.
	if got := effectiveKVCacheTypeForLoad("Qwen3.8-27B", "f16"); got != "f16" {
		t.Errorf("явный параметр: got %q, want f16", got)
	}
}

// TestR83_ProfileLookup_NoSpuriousMatch — правило не подставляет профиль
// наугад: у "qwen3" общий префикс с "qwen3.8", но профиля для чужой модели нет.
func TestR83_ProfileLookup_NoSpuriousMatch(t *testing.T) {
	ps := newSeededProfileSyncerR83()
	for _, name := range []string{"llama-3-8b", "gemma-2-9b", "mistral-7b", "qwen3.8"} {
		if prof := ps.applyProfileOnLoad(name); prof != nil {
			t.Errorf("%q: подставлен чужой профиль (ctx=%d kv=%q)",
				name, prof.ContextLength, prof.KVCacheType)
		}
	}
}

// TestR83_ProfileLookup_GemmaExactBeatsNeighbor — второй живой случай того же
// класса: у gemma-4 и gemma-4-E4B-it-Q4_K_M общий префикс, и каждая модель
// обязана получить СВОЙ профиль, а не соседский по длине ключа (D1).
func TestR83_ProfileLookup_GemmaExactBeatsNeighbor(t *testing.T) {
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{}}
	ps.cache["gemma-4-E4B-it-Q4_K_M"] = types.LlamaCppModelProfile{
		ContextLength: 32768,
		KVCacheType:   "q4_0",
		NumGPULayers:  -1,
	}
	ps.cache["gemma-4"] = types.LlamaCppModelProfile{
		ContextLength: 32768,
		KVCacheType:   "",
		NumGPULayers:  30,
	}
	prof := ps.applyProfileOnLoad("gemma-4")
	if prof == nil {
		t.Fatal("профиль gemma-4 не найден")
	}
	if prof.NumGPULayers != 30 {
		t.Errorf("NumGPULayers = %d, want 30 (профиль gemma-4, а не сосед "+
			"gemma-4-E4B-it-Q4_K_M с -1)", prof.NumGPULayers)
	}
	// Полное имя файла — тоже свой профиль.
	profFile := ps.applyProfileOnLoad("gemma-4-E4B-it-Q4_K_M.gguf")
	if profFile == nil || profFile.KVCacheType != "q4_0" {
		t.Errorf("gemma-4-E4B-it-Q4_K_M.gguf: %+v, want профиль с q4_0", profFile)
	}
}
