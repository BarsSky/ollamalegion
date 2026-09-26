//go:build llama_stub

// sync_profile_lookup_r83_test.go — R83 §3.4 (2026-09-26): живой резолв профиля.
//
// Живой факт со стенда: гейт n_ctx логировал q4_0, а llama.cpp получил
// kv_cache_type=q4_0 — при том, что профиль Qwen3.8-27B в балансере содержит
// kvCacheType=q8_0, и загрузка идёт через /api/models/load-with-params.
//
// Здесь проверяется сам резолв профиля по имени, которым зовёт клиент
// ("qwen3.8:latest"): если он не находится, ни гейт, ни загрузка не увидят
// профиль — и это ровно тот класс ошибки, который ищем.
package main

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newSeededProfileSyncerR83 — syncer с профилями из реального ответа балансера
// (/api/v1/cppworker/model-profiles), без сети.
func newSeededProfileSyncerR83() *profileSyncerT {
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{}}
	q8 := "q8_0"
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
	_ = q8
	return ps
}

// TestR83_ProfileLookup_TaggedName — профиль обязан находиться по имени клиента.
//
// Клиент (Cline/OpenWebUI) зовёт модель "qwen3.8:latest"; профиль в балансере
// называется "Qwen3.8-27B". Если резолв не срабатывает, загрузка теряет и
// kvCacheType=q8_0 из профиля, и его contextLength — то есть вся pull-синхронизация
// профилей для этого имени не работает.
func TestR83_ProfileLookup_TaggedName(t *testing.T) {
	ps := newSeededProfileSyncerR83()
	prof := ps.applyProfileOnLoad("qwen3.8:latest")
	if prof == nil {
		t.Fatalf("профиль не найден для \"qwen3.8:latest\": pull-sync профилей " +
			"не применяется к имени клиента (в кэше: Qwen3.8-27B, Qwen3.6-35B-A3B-UD-Q4_K_M)")
	}
	if prof.KVCacheType != "q8_0" {
		t.Errorf("KVCacheType = %q, want q8_0 (профиль Qwen3.8-27B)", prof.KVCacheType)
	}
}

// TestR83_ProfileLookup_ExactAndCanonical — канонические имена тоже находятся
// (иначе сломается auto-load от балансера, который зовёт модель по имени файла).
func TestR83_ProfileLookup_ExactAndCanonical(t *testing.T) {
	ps := newSeededProfileSyncerR83()
	for _, name := range []string{
		"Qwen3.8-27B",
		"Qwen3.8-27B-UD-Q4_K_M",
		"Qwen3.8-27B-UD-Q4_K_M.gguf",
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

	if got := effectiveKVCacheTypeForLoad("qwen3.8:latest", ""); got != "q8_0" {
		t.Errorf("гейт считает KV типом %q, want q8_0 из профиля (R83 §3.4)", got)
	}
	// Явный параметр запроса по-прежнему главнее профиля.
	if got := effectiveKVCacheTypeForLoad("qwen3.8:latest", "f16"); got != "f16" {
		t.Errorf("явный параметр: got %q, want f16", got)
	}
}

// TestR83_ProfileLookup_DeterministicPick — когда под префикс попадает несколько
// профилей, выбор обязан быть детерминированным (самый длинный ключ), а не
// зависеть от случайного порядка обхода map: иначе один и тот же запрос получал
// бы разные contextLength/kvCacheType от запуска к запуску.
//
// Семантика «самый длинный ключ» = «самый конкретный профиль»: для
// неоднозначного имени клиента ("qwen3.8:latest" ↔ "Qwen3.8-27B" и
// "Qwen3.8-27B-UD-Q4_K_M") выигрывает профиль с более полным именем. Клиенту,
// которому нужен именно короткий профиль, следует звать модель полным именем.
func TestR83_ProfileLookup_DeterministicPick(t *testing.T) {
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{}}
	ps.cache["Qwen3.8-27B"] = types.LlamaCppModelProfile{ContextLength: 32768, KVCacheType: "q8_0"}
	ps.cache["Qwen3.8-27B-UD-Q4_K_M"] = types.LlamaCppModelProfile{ContextLength: 65536, KVCacheType: "f16"}

	for i := 0; i < 50; i++ { // порядок обхода map меняется от запуска к запуску
		prof := ps.applyProfileOnLoad("qwen3.8:latest")
		if prof == nil {
			t.Fatal("профиль не найден")
		}
		if prof.ContextLength != 65536 || prof.KVCacheType != "f16" {
			t.Fatalf("итерация %d: выбран не самый длинный ключ (ctx=%d kv=%q)",
				i, prof.ContextLength, prof.KVCacheType)
		}
	}
}

// TestR83_ProfileLookup_NoSpuriousMatch — нормализация не должна склеивать
// разные модели: у "qwen3" общий префикс с "qwen3.8", но разные модели —
// профиль по несуществующей модели не подставляется наугад.
func TestR83_ProfileLookup_NoSpuriousMatch(t *testing.T) {
	ps := newSeededProfileSyncerR83()
	for _, name := range []string{"llama-3-8b", "gemma-2-9b", "mistral-7b"} {
		if prof := ps.applyProfileOnLoad(name); prof != nil {
			t.Errorf("%q: подставлен чужой профиль (ctx=%d kv=%q)",
				name, prof.ContextLength, prof.KVCacheType)
		}
	}
}

// TestR83_ProfileLookup_GemmaTagged — второй живой случай того же дефекта:
// клиент зовёт "gemma-4:latest", профиль называется "gemma-4-E4B-it-Q4_K_M".
func TestR83_ProfileLookup_GemmaTagged(t *testing.T) {
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{}}
	ps.cache["gemma-4-E4B-it-Q4_K_M"] = types.LlamaCppModelProfile{
		ContextLength: 32768,
		KVCacheType:   "q4_0",
	}
	prof := ps.applyProfileOnLoad("gemma-4:latest")
	if prof == nil {
		t.Fatal("профиль gemma-4-E4B-it-Q4_K_M не найден для \"gemma-4:latest\"")
	}
	if prof.KVCacheType != "q4_0" {
		t.Errorf("KVCacheType = %q, want q4_0", prof.KVCacheType)
	}
}
