// profile_key_d1_test.go — D1 (2026-11-25): правило «имя модели → ключ профиля»
// на стороне воркера.
//
// ЧТО ЗДЕСЬ ДОКАЗЫВАЕТСЯ. До правки резолвер профилей воркера сравнивал
// нормализованные ключи по ПРЕФИКСУ и при нескольких совпадениях брал самый
// длинный ключ. На стенде рядом живут профили-префиксы (Qwen3.8-27B и
// Qwen3.8-27B-UD-Q4_K_M, gemma-4 и gemma-4-E4B-it-Q4_K_M, Qwen3-4B-Instruct-2507
// и Qwen3-4B-Instruct-2507-Q4_K_M), поэтому:
//
//   - запрос по ПОЛНОМУ имени файла (M.gguf) резолвился в СОСЕДНИЙ профиль
//     M-mini (32768) вместо своего M (131072) — «самый длинный ключ» перебивал
//     точное совпадение;
//   - модель БЕЗ своего профиля (M-turbo) получала чужой профиль по общему
//     префиксу M → 131072;
//   - профиль, сохранённый С расширением (записи до R136), находился только
//     случайно — через тот же префиксный поиск.
//
// Все четыре проверки ниже на состоянии до правки КРАСНЫЕ (проверено прогоном на
// 5d30e58f/bab70132 с временно откаченным резолвером), после правки — зелёные.
package main

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// profileCacheD1 — профильный набор стенда в миниатюре: модель и её
// префикс-сосед. Именно такая пара (Qwen3.8-27B / Qwen3.8-27B-UD-Q4_K_M) дала
// живой дефект D1.
func profileCacheD1() map[string]types.LlamaCppModelProfile {
	return map[string]types.LlamaCppModelProfile{
		"M":      {ContextLength: 131072, KVCacheType: "q8_0"},
		"M-mini": {ContextLength: 32768, KVCacheType: "q4_0"},
	}
}

// ctxD1 — контекст профиля, найденного для имени (0 = профиля нет).
func ctxD1(ps *profileSyncerT, name string) int {
	if prof := ps.applyProfileOnLoad(name); prof != nil {
		return prof.ContextLength
	}
	return 0
}

// TestD1_ProfileLookup_FullFileNameFindsProfileWithoutExtension — профиль
// заведён БЕЗ расширения, а спрашивают ПОЛНЫМ именем файла.
//
// До правки красный: у этого имени нет точного ключа, срабатывал префиксный
// поиск, и «самый длинный ключ» выбирал соседа M-mini → 32768.
func TestD1_ProfileLookup_FullFileNameFindsProfileWithoutExtension(t *testing.T) {
	ps := &profileSyncerT{cache: profileCacheD1()}
	for _, name := range []string{
		"M",
		"M.gguf",
		"models/M.gguf",
		"/app/models/M.gguf",
		`C:\models\M.gguf`, // Windows-разделители (R245)
	} {
		if got := ctxD1(ps, name); got != 131072 {
			t.Errorf("%q: ctx = %d, want 131072 (профиль M): имя файла обязано "+
				"находиться, и сосед M-mini (32768) не должен его перебивать", name, got)
		}
	}
}

// TestD1_ProfileLookup_KeyWithGGUFIsFound — обратный случай совместимости:
// ключ профиля сохранён С расширением (записи до R136), спрашивают без него.
//
// До правки красный: точного ключа нет, префиксный поиск снова выбирал соседа.
func TestD1_ProfileLookup_KeyWithGGUFIsFound(t *testing.T) {
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{
		"M.gguf":  {ContextLength: 131072, KVCacheType: "q8_0"},
		"M-other": {ContextLength: 32768, KVCacheType: "q4_0"},
	}}
	for _, name := range []string{"M", "M.gguf", "models/M.gguf"} {
		if got := ctxD1(ps, name); got != 131072 {
			t.Errorf("%q: ctx = %d, want 131072 (ключ профиля с .gguf): записи до "+
				"R136 обязаны читаться, сосед M-other не должен подставляться", name, got)
		}
	}
}

// TestD1_ProfileLookup_NoProfileNoForeign — у модели СВОЕГО профиля нет:
// правильный ответ — «профиля нет», а не профиль соседа по префиксу.
//
// До правки красный: M-turbo получал профиль M (131072) только потому, что
// нормализованный ключ M — префикс ключа M-turbo.
func TestD1_ProfileLookup_NoProfileNoForeign(t *testing.T) {
	ps := &profileSyncerT{cache: profileCacheD1()}
	for _, name := range []string{
		"M-turbo",
		"M-turbo.gguf",
		"models/M-turbo.gguf",
		"Mmini", // ключ M-mini не должен склеиваться с Mmini
	} {
		if prof := ps.applyProfileOnLoad(name); prof != nil {
			t.Errorf("%q: подставлен ЧУЖОЙ профиль (ctx=%d kv=%q); правильный ответ — "+
				"профиля нет: чужой профиль молча подменяет параметры оператора",
				name, prof.ContextLength, prof.KVCacheType)
		}
	}
}

// TestD1_ProfileLookup_PrefixNeighborNotSubstituted — профиль-префикс рядом не
// подставляется: и «короткая» модель, и её сосед берут КАЖДАЯ свой профиль.
//
// До правки красный на первом же кейсе: для M.gguf нормализованный ключ M
// совпадал с M-mini по префиксу, и выбирался более длинный ключ M-mini (32768) —
// живой симптом `profile_n_ctx=32768` при профиле 131072.
func TestD1_ProfileLookup_PrefixNeighborNotSubstituted(t *testing.T) {
	ps := &profileSyncerT{cache: profileCacheD1()}
	cases := []struct {
		name string
		want int
		why  string
	}{
		{"M", 131072, "свой профиль (точное имя)"},
		{"M.gguf", 131072, "свой профиль (имя файла)"},
		{"M-mini", 32768, "свой профиль соседа — он существует и не должен теряться"},
		{"M-mini.gguf", 32768, "сосед по имени файла"},
	}
	for _, c := range cases {
		if got := ctxD1(ps, c.name); got != c.want {
			t.Errorf("%q: ctx = %d, want %d (%s)", c.name, got, c.want, c.why)
		}
	}
}

// TestD1_ProfileLookup_LiveStandKeys — набор профилей ЖИВОГО стенда (21 запись,
// снят с /api/v1/cppworker/model-profiles): проверяем, что полное имя модели
// находит свой профиль 131072, а не соседний Qwen3.8-27B (32768).
func TestD1_ProfileLookup_LiveStandKeys(t *testing.T) {
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{
		"Qwen3.8-27B":                   {ContextLength: 32768, KVCacheType: "q8_0"},
		"Qwen3.8-27B-UD-Q4_K_M":         {ContextLength: 131072, KVCacheType: "q8_0"},
		"Qwen3-4B-Instruct-2507":        {ContextLength: 32768, KVCacheType: "f16"},
		"Qwen3-4B-Instruct-2507-Q4_K_M": {ContextLength: 32768, KVCacheType: "f16"},
		"gemma-4":                       {ContextLength: 32768, KVCacheType: ""},
		"gemma-4-E4B-it-Q4_K_M":         {ContextLength: 32768, KVCacheType: "q4_0"},
	}}
	for _, name := range []string{
		"Qwen3.8-27B-UD-Q4_K_M",
		"Qwen3.8-27B-UD-Q4_K_M.gguf",
	} {
		if got := ctxD1(ps, name); got != 131072 {
			t.Errorf("%q: ctx = %d, want 131072 (живой дефект D1: сосед Qwen3.8-27B "+
				"даёт 32768, и балансер с воркером расходятся по окну)", name, got)
		}
	}
	// Профиль по короткому имени остаётся своим: точное совпадение главнее
	// соседского более длинного ключа.
	if got := ctxD1(ps, "Qwen3.8-27B"); got != 32768 {
		t.Errorf("Qwen3.8-27B: ctx = %d, want 32768 (свой профиль короткой модели)", got)
	}
}

// TestD1_ProfileLookup_FoldIsDeterministic — последний шаг лестницы (регистр)
// обязан быть детерминированным: порядок обхода map в Go случаен, поэтому при
// двух ключах, отличающихся только регистром, выбор фиксируется сортировкой.
func TestD1_ProfileLookup_FoldIsDeterministic(t *testing.T) {
	ps := &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{
		"Qwen3.8-27B-UD": {ContextLength: 131072},
		"qwen3.8-27b-ud": {ContextLength: 32768},
	}}
	// "Qwen3.8-27B-UD" < "qwen3.8-27b-ud" лексикографически (верхний регистр
	// раньше строчного), поэтому побеждает первый ключ.
	for i := 0; i < 50; i++ {
		if got := ctxD1(ps, "QWEN3.8-27B-UD"); got != 131072 {
			t.Fatalf("итерация %d: ctx = %d, want 131072 (детерминированный выбор "+
				"при совпадении без учёта регистра)", i, got)
		}
	}
}

// TestD1_ProfileLookupCandidates — сама лестница: порядок и состав вариантов.
//
// Правило обязано быть зеркалом window.GgufModelKey (R692): точное имя →
// basename → +".gguf" → канонический ключ; подстрок и префиксов нет.
func TestD1_ProfileLookupCandidates(t *testing.T) {
	got := profileLookupCandidates("/app/models/M.gguf")
	want := []string{"/app/models/M.gguf", "M.gguf", "M"}
	if len(got) != len(want) {
		t.Fatalf("кандидаты = %+v, want %v", got, want)
	}
	for i := range want {
		if got[i].Key != want[i] {
			t.Errorf("шаг %d: %q, want %q (вся лестница: %+v)", i, got[i].Key, want[i], got)
		}
	}
	if got[len(got)-1].How != profileMatchNormalized {
		t.Errorf("последний шаг = %q, want %q", got[len(got)-1].How, profileMatchNormalized)
	}
	// Имя без расширения не даёт «двойного .gguf», а имя `gguf` — не расширение.
	if keys := profileLookupCandidates("M"); len(keys) != 2 || keys[1].Key != "M.gguf" {
		t.Errorf("кандидаты(M) = %+v, want [M M.gguf]", keys)
	}
	if keys := profileLookupCandidates("gguf"); len(keys) != 2 || keys[1].Key != "gguf.gguf" {
		t.Errorf("кандидаты(gguf) = %+v: имя `gguf` — не расширение (len>5), лишний "+
			"суффикс добавляется", keys)
	}
}
