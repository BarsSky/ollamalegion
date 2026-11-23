// kv_plan_consumer_r707_test.go — R707 (2026-11-25): потребитель раскладки KV в
// балансере (R659 и AutoTune).
//
// ПРЕДМЕТ. Балансер решает «поднимется ли модель с нужным окном на свободном узле»
// (R659) и «какой KV-тип ставить» (AutoTune) по kvCacheBytesForContext, которая до
// R707 считала KV как NLayers × NKvHeads × HeadDimK × n_ctx — по ВСЕМ слоям.
// У gemma-4-E4B это 3.17 GiB на окно 65536 (q4_0) против реальных ~302 MiB:
// узел с 7.1 ГБ свободной VRAM получал вердикт «окно не влезает» и загрузка не
// начиналась, хотя модель туда физически помещается.
//
// НЕ-ВАКУУМНОСТЬ. Оба вердикта (plan и flat) считаются одной и той же функцией на
// одних и тех же входах, кроме раскладки; проверяется, что они РАЗНЫЕ, а числа
// KV записаны вручную (302 MiB против 3.17 GiB).
package balancer

import (
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// readFileR707 — исходник балансера для сторожа провода.
func readFileR707(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}

// gemma4ProfileR707 — профиль gemma-4-E4B из /api/models: 42 блока, 2 KV-головы,
// key_length 512, и раскладка (24 слоя с кэшем: 4 глобальных + 20 оконных).
func gemma4ProfileR707(withPlan bool) *ModelProfileInfo {
	p := &ModelProfileInfo{
		Name:           "gemma-4-E4B-it-Q4_K_M",
		Architecture:   "gemma4",
		NLayers:        42,
		NEmbd:          2560,
		NKvHeads:       2,
		HeadDimK:       512,
		HeadDimV:       512,
		SizeBytes:      4215695776,
		GgufMaxContext: 131072,
	}
	if withPlan {
		p.KVPlanSource = "plan"
		p.KVGlobalLayers = 4
		p.KVSWALayers = 20
		p.KVSWAWindow = 512
		p.HeadDimSWA = 256
		p.HeadDimSWAV = 256
		p.NKvHeadsSWA = 2
	}
	return p
}

// TestR707_BalancerKVUsesLayerPlan — KV на окно 65536 в разы меньше плоского.
//
// Ручной расчёт q4_0 (18/32 байта на элемент):
//
//	плоско: 2·42·2·512 элементов = 86 016  → 48 384 Б/токен × 65536 = 3 170 893 824
//	план:   глобальные 4·2·(512+512)=8192 → 4 608 Б/токен × 65536 =   301 989 888
//	        оконные  20·2·(256+256)=20480 → 11 520 Б/токен × 512 =      5 898 240
//	        итого 307 888 128 Б (~294 MiB)
func TestR707_BalancerKVUsesLayerPlan(t *testing.T) {
	const nCtx = 65536
	withPlan := kvCacheBytesForContext(gemma4ProfileR707(true), "q4_0", nCtx)
	flat := kvCacheBytesForContext(gemma4ProfileR707(false), "q4_0", nCtx)

	if want := uint64(307888128); withPlan != want {
		t.Fatalf("KV с раскладкой = %d, want %d", withPlan, want)
	}
	if want := uint64(3170893824); flat != want {
		t.Fatalf("KV плоский = %d, want %d", flat, want)
	}
	// НЕ-ВАКУУМНОСТЬ: раскладка меняет число в 10 раз.
	if flat < withPlan*9 {
		t.Errorf("плоский KV %d должен быть не менее чем в 9 раз больше %d", flat, withPlan)
	}
}

// TestR707_R659WindowFitUsesPlan — вердикт R659 переворачивается.
//
// Узел: свободно 7097 МБ, веса 4020 МБ, резерв 256 МБ, один слот, окно 65536.
//
//	плоско: KV 3024 МБ → нужно 7300 МБ → НЕ влезает;
//	план:   KV  293 МБ → нужно 4569 МБ → влезает.
func TestR707_R659WindowFitUsesPlan(t *testing.T) {
	const (
		window  = 65536
		weights = 4020
		freeMB  = 7097
		reserve = 256
		slots   = 1
	)
	plan := coldLoadWindowFitPartsR659(gemma4ProfileR707(true), "q4_0", window, weights, freeMB, reserve, slots)
	flat := coldLoadWindowFitPartsR659(gemma4ProfileR707(false), "q4_0", window, weights, freeMB, reserve, slots)

	if !plan.Verified || !flat.Verified {
		t.Fatalf("вердикт не верифицирован: plan=%+v flat=%+v", plan, flat)
	}
	if plan.KVMB != 293 {
		t.Errorf("KVMB с раскладкой = %d, want 294", plan.KVMB)
	}
	if flat.KVMB != 3024 {
		t.Errorf("KVMB плоский = %d, want 3025", flat.KVMB)
	}
	if !plan.Fits {
		t.Errorf("с раскладкой окно %d должно влезать: need=%d free=%d", window, plan.NeedMB, freeMB)
	}
	if flat.Fits {
		t.Errorf("по плоской формуле окно не влезает, а вердикт «влезает»: need=%d free=%d",
			flat.NeedMB, freeMB)
	}
	if plan.Fits == flat.Fits {
		t.Fatal("вердикт R659 одинаков с раскладкой и без — изменение не влияет на решение")
	}
}

// TestR707_ProfileGetsPlanFromModel — раскладка переносится из модели воркера в
// профиль ОДНОЙ функцией (четыре места сборки профиля — один перенос).
func TestR707_ProfileGetsPlanFromModel(t *testing.T) {
	m := &types.LlamaCppModel{
		Name:           "gemma-4-E4B-it-Q4_K_M",
		NLayers:        42,
		NKvHeads:       2,
		HeadDimK:       512,
		HeadDimV:       512,
		KVPlanSource:   "plan",
		KVGlobalLayers: 4,
		KVSWALayers:    20,
		KVSWAWindow:    512,
		HeadDimSWA:     256,
		HeadDimSWAV:    256,
		NKvHeadsSWA:    2,
	}
	prof := &ModelProfileInfo{Name: m.Name, NLayers: m.NLayers, NKvHeads: m.NKvHeads, HeadDimK: m.HeadDimK, HeadDimV: m.HeadDimV}
	applyKVPlanToProfileR707(prof, m)
	if prof.KVPlanSource != "plan" || prof.KVGlobalLayers != 4 || prof.KVSWALayers != 20 ||
		prof.KVSWAWindow != 512 || prof.HeadDimSWA != 256 || prof.NKvHeadsSWA != 2 {
		t.Fatalf("раскладка не перенесена: %+v", prof)
	}

	// Старый cppworker (без полей) — профиль остаётся плоским, KV считается как раньше.
	old := &types.LlamaCppModel{Name: "old", NLayers: 36, NKvHeads: 8, HeadDimK: 128, HeadDimV: 128}
	profOld := &ModelProfileInfo{Name: "old", NLayers: 36, NKvHeads: 8, HeadDimK: 128, HeadDimV: 128}
	applyKVPlanToProfileR707(profOld, old)
	if profOld.KVPlanSource != "" {
		t.Errorf("KVPlanSource = %q для старого воркера, want пусто", profOld.KVPlanSource)
	}
	if got, want := kvCacheBytesForContext(profOld, "f16", 16384),
		uint64(2*36*8*128)*2*16384; got != want {
		t.Errorf("плоский KV = %d, want %d (прежняя формула)", got, want)
	}
}

// TestR707_WireNamesMatchWorker — сторож провода: имена полей раскладки в
// /api/models воркера и в поллере балансера обязаны совпадать.
//
// Переименование любой стороны молча вернёт «раскладки нет», и R659 снова начнёт
// считать KV по всем слоям — ровно этот класс дефекта уже был в R660/R45.
func TestR707_WireNamesMatchWorker(t *testing.T) {
	raw, err := readFileR707("llamacpp_metrics_poller.go")
	if err != nil {
		t.Fatalf("не читается поллер: %v", err)
	}
	text := strings.Join(strings.Fields(raw), " ")
	for _, wire := range []string{
		`kv_plan_source`, `kv_global_layers`, `kv_swa_layers`,
		`kv_swa_window`, `head_dim_swa`, `head_dim_swa_v`, `n_kv_heads_swa`,
	} {
		if !strings.Contains(text, wire) {
			t.Errorf("поллер не читает поле %q из /api/models", wire)
		}
	}
	for _, dst := range []string{
		"KVPlanSource: m.KVPlanSource", "KVGlobalLayers: m.KVGlobalLayers",
		"KVSWALayers: m.KVSWALayers", "KVSWAWindow: m.KVSWAWindow",
		"HeadDimSWA: m.HeadDimSWA", "HeadDimSWAV: m.HeadDimSWAV",
		"NKvHeadsSWA: m.NKvHeadsSWA",
	} {
		if !strings.Contains(text, dst) {
			t.Errorf("поллер не переносит %q в types.LlamaCppModel", dst)
		}
	}
}
