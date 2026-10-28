package balancer

// nctx_declared_r255_test.go — R255 (2026-10-28): «заявлено 256000 → загружено
// 32768» и «рост больше 131072 невозможен».
//
// Тесты фиксируют две вещи:
//  1. Два поля профиля про одно и то же окно (суммарное `contextLength` и
//     «на клиента» `contextPerSeq`) больше не могут МОЛЧА перебить друг друга:
//     устаревшее per-seq не режет заявленное суммарное окно (живой случай
//     16384 × 2 слота = 32768 вместо 256000), а противоречие печатается одной
//     строкой с числами.
//  2. Потолок АВТО-роста (LB_NCTX_RELOAD_MAX_N_CTX) не отменяет решение
//     оператора: если в профиле заявлено окно больше, потолок поднимается до
//     заявленного — иначе агент получает отказ там, где VRAM хватает.

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR255_StalePerSeqDoesNotShrinkDeclaredTotal — главный живой случай.
func TestR255_StalePerSeqDoesNotShrinkDeclaredTotal(t *testing.T) {
	// Оператор заявил суммарное 256000, в профиле осталось per-seq 16384,
	// слотов 2 → воркер посчитал бы 32768 и «проигнорировал» настройку.
	total, perSeq, note := resolveDeclaredWindow(256000, 16384, 2)
	if total != 256000 {
		t.Fatalf("суммарное окно = %d, want 256000 (заявленное оператором)", total)
	}
	if perSeq != 0 {
		t.Fatalf("per-seq = %d, want 0: устаревшее per-seq нельзя отправлять — воркер пересчитает окно сам", perSeq)
	}
	if note == "" {
		t.Fatal("противоречие полей обязано объясняться строкой, иначе оно снова станет молчаливым")
	}
}

// TestR255_ConsistentFieldsUnchanged — согласованная пара работает как раньше.
func TestR255_ConsistentFieldsUnchanged(t *testing.T) {
	total, perSeq, note := resolveDeclaredWindow(32768, 16384, 2)
	if total != 32768 || perSeq != 16384 {
		t.Fatalf("согласованная пара изменилась: total=%d perSeq=%d", total, perSeq)
	}
	if note != "" {
		t.Errorf("для согласованной пары строка-объяснение не нужна: %q", note)
	}
	// Выравнивание воркера (256 на слот) не должно считаться противоречием.
	if _, _, note := resolveDeclaredWindow(33280, 16640, 2); note != "" {
		t.Errorf("расхождение в пределах выравнивания — не противоречие: %q", note)
	}
}

// TestR255_TotalWinsWhenBothDiffer — суммарное авторитетно в ОБЕ стороны.
//
// Почему в обе: «большее» было бы угадыванием за оператора. Если he задал
// суммарное 32768, а per-seq осталось 65536 (старый опыт), рост до 131072 сверх
// заявленного — лишняя VRAM и риск OOM; если наоборот (заявлено 256000, per-seq
// 16384) — обрезка до 32768, то есть ровно живая жалоба. Поэтому выигрывает
// поле, которое оператор и правит в карточке — суммарное.
func TestR255_TotalWinsWhenBothDiffer(t *testing.T) {
	total, perSeq, note := resolveDeclaredWindow(32768, 65536, 2)
	if total != 32768 {
		t.Fatalf("суммарное = %d, want 32768 (заявлено оператором)", total)
	}
	if perSeq != 0 {
		t.Fatalf("per-seq = %d, want 0 (противоречие в пользу суммарного)", perSeq)
	}
	if note == "" {
		t.Error("противоречие полей обязано объясняться")
	}
}

// TestR255_OnlyOneFieldSet — одно поле не выдумывает второе.
func TestR255_OnlyOneFieldSet(t *testing.T) {
	if total, perSeq, _ := resolveDeclaredWindow(262144, 0, 2); total != 262144 || perSeq != 0 {
		t.Fatalf("только суммарное: got total=%d perSeq=%d", total, perSeq)
	}
	// Только per-seq: суммарное ВЫЧИСЛЯЕТСЯ как per-seq × слоты (это контракт
	// воркера, R83), а не подставляется как есть.
	if total, perSeq, _ := resolveDeclaredWindow(0, 262144, 2); total != 524288 || perSeq != 262144 {
		t.Fatalf("только per-seq: got total=%d perSeq=%d, want 524288/262144", total, perSeq)
	}
	if total, perSeq, _ := resolveDeclaredWindow(0, 0, 2); total != 0 || perSeq != 0 {
		t.Fatal("пустой профиль не должен давать окно")
	}
}

// TestR255_DeclaredWindowRaisesAutoGrowthCeiling — второй симптом жалобы.
func TestR255_DeclaredWindowRaisesAutoGrowthCeiling(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"qwen3.8-27b": {ContextLength: 256000, ContextPerSeq: 131072, Parallel: 2},
		},
	}}

	// Заявлено 256000, потолок авто-роста 131072 → потолок поднимается до
	// заявленного, иначе агент получает отказ там, где окно помещается.
	if got := p.effectiveAutoReloadCeiling("qwen3.8-27b", 131072); got != 256000 {
		t.Fatalf("потолок = %d, want 256000 (заявленное окно оператора)", got)
	}

	// Меньший потолок не понижает заявленное окно (политика оператора сильнее).
	if got := p.effectiveAutoReloadCeiling("qwen3.8-27b", 32768); got != 256000 {
		t.Fatalf("потолок = %d, want 256000", got)
	}

	// Потолок не задан (0 = без ограничения) — остаётся «без ограничения».
	if got := p.effectiveAutoReloadCeiling("qwen3.8-27b", 0); got != 0 {
		t.Fatalf("без потолка = %d, want 0", got)
	}

	// Модель без профиля: ведёт себя как раньше (env-потолок).
	if got := p.effectiveAutoReloadCeiling("нет-такой", 131072); got != 131072 {
		t.Fatalf("без профиля = %d, want 131072 (env-потолок)", got)
	}

	// per-seq больше суммарного — потолок берёт большее из двух.
	p.config.LlamaCppModelProfiles["big"] = types.LlamaCppModelProfile{ContextLength: 0, ContextPerSeq: 98304, Parallel: 2}
	if got := p.effectiveAutoReloadCeiling("big", 131072); got != 196608 {
		t.Fatalf("по per-seq = %d, want 196608 (98304 × 2)", got)
	}
}

// TestR255_NilProxyIsSafe — потолок для nil-прокси не паникует (страховка для
// тестов и ранних вызовов).
func TestR255_NilProxyIsSafe(t *testing.T) {
	var p *Proxy
	if got := p.effectiveAutoReloadCeiling("m", 131072); got != 131072 {
		t.Fatalf("nil-proxy = %d, want 131072", got)
	}
}

// TestR255_Pad256 — выравнивание как у воркера.
func TestR255_Pad256(t *testing.T) {
	cases := map[int]int{0: 0, 1: 256, 256: 256, 257: 512, 16384: 16384, 16640: 16640}
	for in, want := range cases {
		if got := pad256(in); got != want {
			t.Errorf("pad256(%d) = %d, want %d", in, got, want)
		}
	}
}
