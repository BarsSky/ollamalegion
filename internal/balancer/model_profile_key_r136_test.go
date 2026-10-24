//go:build llama_stub

// model_profile_key_r136_test.go — R136 (2026-10-16): карточка модели на диске и
// карточка загруженной модели пишут ОДНУ запись профиля.
//
// ЖИВОЙ ДЕФЕКТ (воспроизведён сквозным тестом от лица WebUI, Playwright):
// карточка на диске называет модель именем файла (`gemma-4-E4B-it-Q4_K_M.gguf`),
// а путь загрузки ищет профиль по имени модели без расширения. На стенде в
// хранилище оказались ОБА ключа одновременно:
//
//	gemma-4-E4B-it-Q4_K_M.gguf -> {parallel:2, enableReasoning:true}   (карточка диска)
//	gemma-4-E4B-it-Q4_K_M      -> {parallel:1, enableReasoning:false}  (читает загрузка)
//
// Следствие для оператора: «выставил параметры, применил — а модель поднялась с
// совсем другими (дефолтными) значениями».
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func boolRefR136(v bool) *bool { return &v }

// TestR136_ProfileKeyNormalization — что считается одним и тем же ключом.
func TestR136_ProfileKeyNormalization(t *testing.T) {
	cases := map[string]string{
		"gemma-4-E4B-it-Q4_K_M.gguf":     "gemma-4-E4B-it-Q4_K_M",
		"gemma-4-E4B-it-Q4_K_M":          "gemma-4-E4B-it-Q4_K_M",
		"/app/models/gemma-4.gguf":       "gemma-4",
		`D:\models\gemma-4.gguf`:         "gemma-4",
		"GEMMA-4.GGUF":                   "GEMMA-4",
		"  gemma-4.gguf  ":               "gemma-4",
		"":                               "",
		"gguf":                           "gguf", // не расширение — имя модели
	}
	for in, want := range cases {
		if got := NormalizeModelProfileKey(in); got != want {
			t.Errorf("NormalizeModelProfileKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestR136_DiskCardAndLoadedCardShareOneProfile — ГЛАВНОЕ: запись из карточки
// диска доступна загрузке (она читает профиль по имени модели без расширения).
func TestR136_DiskCardAndLoadedCardShareOneProfile(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}

	// Так пишет карточка модели НА ДИСКЕ (имя файла).
	p.SetModelProfile("gemma-4-E4B-it-Q4_K_M.gguf", types.LlamaCppModelProfile{
		ContextLength:   8192,
		Parallel:        2,
		ContextPerSeq:   4096,
		EnableReasoning: boolRefR136(true),
	})

	// Так это читает путь загрузки — ПРЯМО из карты, по имени модели.
	direct, ok := p.config.LlamaCppModelProfiles["gemma-4-E4B-it-Q4_K_M"]
	if !ok {
		t.Fatalf("загрузка не видит профиль, записанный карточкой диска: ключи = %v", profileKeysR136(p))
	}
	if direct.Parallel != 2 || direct.ContextPerSeq != 4096 ||
		direct.EnableReasoning == nil || !*direct.EnableReasoning {
		t.Fatalf("загрузка видит НЕ те параметры: %+v", direct)
	}

	// И API читает ту же запись по любому из имён.
	for _, name := range []string{"gemma-4-E4B-it-Q4_K_M", "gemma-4-E4B-it-Q4_K_M.gguf"} {
		got, ok := p.GetModelProfile(name)
		if !ok || got.Parallel != 2 {
			t.Errorf("GetModelProfile(%q) = %+v, ok=%v — ожидали ту же запись", name, got, ok)
		}
	}
	if len(p.config.LlamaCppModelProfiles) != 1 {
		t.Errorf("в хранилище %d записей вместо одной: %v", len(p.config.LlamaCppModelProfiles), profileKeysR136(p))
	}
}

// TestR136_LegacySuffixedProfileIsStillReadable — совместимость: запись осталась
// под старым ключом с расширением (до R136) — читаем её, чтобы настройки
// оператора не потерялись.
func TestR136_LegacySuffixedProfileIsStillReadable(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
		"legacy.gguf": {ContextLength: 16384},
	}}}
	got, ok := p.GetModelProfile("legacy")
	if !ok || got.ContextLength != 16384 {
		t.Fatalf("запись со старым ключом не читается по имени без расширения: %+v ok=%v", got, ok)
	}
}

// TestR136_DeleteRemovesBothForms — «снять профиль» из карточки диска не должен
// оставлять мёртвую запись.
func TestR136_DeleteRemovesBothForms(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	p.SetModelProfile("m", types.LlamaCppModelProfile{ContextLength: 8192})
	// кладём дубль вручную, как это выглядело до R136
	p.config.LlamaCppModelProfiles["m.gguf"] = types.LlamaCppModelProfile{ContextLength: 4096}
	if !p.DeleteModelProfile("m.gguf") {
		t.Fatal("DeleteModelProfile вернул false, хотя записи есть")
	}
	if len(p.config.LlamaCppModelProfiles) != 0 {
		t.Fatalf("после удаления остались записи: %v", profileKeysR136(p))
	}
}

func profileKeysR136(p *Proxy) []string {
	out := make([]string, 0, len(p.config.LlamaCppModelProfiles))
	for k := range p.config.LlamaCppModelProfiles {
		out = append(out, k)
	}
	return out
}
