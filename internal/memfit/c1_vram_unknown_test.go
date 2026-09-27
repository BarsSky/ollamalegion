// c1_vram_unknown_test.go — R83 C1 (2026-09-27).
//
// РЕШЕНИЕ ВЛАДЕЛЬЦА: «всё же стоит учитывать явно доступную память — как он может
// грузить в видеокарту, не зная, сколько у него доступно памяти».
//
// То есть cpu_only — это утверждение «в VRAM известно сколько, и туда не влезает
// ни один слой». Когда VRAM НЕИЗВЕСТНА, такого утверждения сделать нельзя, и
// понижать раскладку до CPU-only нельзя тем более: на живом стенде это означало
// бы «NVML отвалился → модель поехала на CPU, хотя карта свободна».
//
// Что было до правки: при VRAMKnown=false UsableVRAM=0 → computeSplit давал
// gMax=0 → Stage=cpu_only. Реason vram_unknown при этом уже стоял, но решение
// всё равно принималось за оператора.
package memfit

import "testing"

// TestC1_UnknownVRAMIsNotCPUOnly — главный инвариант: неизвестная VRAM не даёт
// стадию cpu_only.
func TestC1_UnknownVRAMIsNotCPUOnly(t *testing.T) {
	b := Budget{
		RAMTotal: GiBOf(24), RAMAvail: GiBOf(20), RAMKnown: true,
		VRAMKnown: false, // GPU есть, но его размер неизвестен
	}
	v := Evaluate(qwen38, Request{Ctx: 8192, KVType: KVQ8}, b, Policy{VRAMUtil: 1, RAMUtil: 1})

	if v.Stage == StageCPUOnly {
		t.Fatalf("Stage = cpu_only при НЕИЗВЕСТНОЙ VRAM (suggestion=%q) — решение принято "+
			"за оператора: на живом стенде это уводит модель на CPU из-за отвалившегося NVML", v.Suggestion)
	}
	if v.Stage != StagePartial {
		t.Errorf("Stage = %s, want partial_offload (раскладку определит C-сторона при загрузке)", v.Stage)
	}
	if !v.HasReason(ReasonVRAMUnknown) {
		t.Error("нет reason=vram_unknown — оператор не поймёт, почему решение неполное")
	}
	if !v.Fits() {
		t.Error("вердикт не fits — загрузка будет отвергнута из-за неизвестной VRAM")
	}
}

// TestC1_KnownVRAMThatCannotHoldWeightsIsCPUOnly — обратная сторона: когда VRAM
// ИЗВЕСТНА и в неё не влезает ни один слой, cpu_only остаётся правильным ответом
// (и оператор должен получить об этом предупреждение).
func TestC1_KnownVRAMThatCannotHoldWeightsIsCPUOnly(t *testing.T) {
	// VRAM ИЗВЕСТНА и в ней есть место (192 MiB после резерва 64 MiB), но один
	// слой весит SizeBytes/NLayers = 15.33 GiB / 65 ≈ 241 MiB, то есть не влезает
	// даже он; RAM 20 GiB — хватает. Резервы заданы явно и маленькими, иначе
	// (при дефолтных 2 GiB на такой карте) usable VRAM = 0, и тест проверял бы
	// вырожденный случай «резерв съел карту целиком», а не «известно, что слои не
	// влезают».
	b := Budget{
		VRAMTotal: MiBOf(256), VRAMFree: MiBOf(256), VRAMKnown: true, VRAMReserve: MiBOf(64),
		RAMTotal: GiBOf(24), RAMAvail: GiBOf(20), RAMKnown: true,
	}
	v := Evaluate(qwen38, Request{Ctx: 8192, KVType: KVQ8}, b, Policy{VRAMUtil: 1, RAMUtil: 1})

	if v.Stage != StageCPUOnly {
		t.Fatalf("Stage = %s, want cpu_only: VRAM известна (%s доступно) и веса %s в неё не влезают "+
			"ни одним слоем (suggestion=%q)", v.Stage, v.UsableVRAM, qwen38.SizeBytes, v.Suggestion)
	}
	if v.GPULayers != 0 {
		t.Errorf("GPULayers = %d, want 0", v.GPULayers)
	}
}

// TestC1_UnknownVRAMInBothResourcesStaysUnknown — если неизвестны и VRAM, и RAM,
// стадия unknown, а не cpu_only: судить не о чем.
func TestC1_UnknownVRAMInBothResourcesStaysUnknown(t *testing.T) {
	v := Evaluate(qwen38, Request{Ctx: 8192, KVType: KVQ8}, Budget{},
		Policy{VRAMUtil: 1, RAMUtil: 1, UnknownIsFatal: false})
	if v.Stage != StageUnknown {
		t.Errorf("Stage = %s, want unknown (нет данных ни о VRAM, ни о RAM)", v.Stage)
	}
}
