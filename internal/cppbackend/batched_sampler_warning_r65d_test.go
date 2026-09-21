//go:build llama_stub

// batched_sampler_warning_r65d_test.go — R65d (2026-09-20): batched-режим
// игнорирует sampler-параметры, и это должно быть ВИДНО.
//
// Контекст (аудит 2026-09-20): batched_scheduler сэмплит через
// sampleFromLogits(model, logits, Temperature, Seed), то есть учитывает только
// temperature и seed. Все остальные sampler-настройки запроса
// (top_p/top_k/min_p/typical_p/tfs_z/repeat_penalty/frequency_penalty/
// presence_penalty/repeat_last_n/mirostat*/stop) молча игнорировались — без
// единой записи в логе.
//
// Практический эффект: клиент (OpenWebUI/Cline) задавал, например,
// top_p=0.5 и stop=["</s>"], а получал другое распределение и отсутствие
// ранней停止. Диагностировать такое поведение по логам было невозможно.
//
// Тест фиксирует детектор: функция должна вернуть ИМЕНА проигнорированных
// параметров, а на дефолтных значениях — пустой список (иначе предупреждение
// спамило бы на каждом запросе).
package cppbackend

import (
	"reflect"
	"sort"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// TestR65d_UnsupportedBatchedSamplerParams_DefaultsSilent — на дефолтных
// значениях предупреждения быть не должно.
func TestR65d_UnsupportedBatchedSamplerParams_DefaultsSilent(t *testing.T) {
	def := bridge.DefaultGenerationParams()
	params := BatchedSessionParams{
		MaxTokens:        def.NPredict,
		Temperature:      def.Temperature,
		TopP:             def.TopP,
		TopK:             def.TopK,
		MinP:             def.MinP,
		TypicalP:         def.TypicalP,
		TfsZ:             def.TfsZ,
		RepeatPenalty:    def.RepeatPenalty,
		FrequencyPenalty: def.FrequencyPenalty,
		PresencePenalty:  def.PresencePenalty,
		RepeatLastN:      def.RepeatLastN,
		Mirostat:         def.Mirostat,
		MirostatTau:      def.MirostatTau,
		MirostatEta:      def.MirostatEta,
	}
	if got := unsupportedBatchedSamplerParams(params); len(got) != 0 {
		t.Errorf("на дефолтных параметрах ожидался пустой список, got %v "+
			"(предупреждение спамило бы на каждом запросе)", got)
	}

	// Нулевые значения (клиент ничего не задал) — тоже молча.
	//
	// ВАЖНО: включая TopK=0. У bridge дефолт top_k = 40, но сюда приходит
	// ЗНАЧЕНИЕ ИЗ ЗАПРОСА: 0 = «клиент не задал», и bridge подставит дефолт.
	// Поэтому 0 не считается переопределением — иначе предупреждение сыпалось бы
	// на каждом запросе без явного top_k.
	if got := unsupportedBatchedSamplerParams(BatchedSessionParams{}); len(got) != 0 {
		t.Errorf("на нулевых параметрах ожидался пустой список, got %v", got)
	}
}

// TestR65d_UnsupportedBatchedSamplerParams_DetectsOverrides — реальные
// переопределения должны быть перечислены.
func TestR65d_UnsupportedBatchedSamplerParams_DetectsOverrides(t *testing.T) {
	params := BatchedSessionParams{
		TopP:          0.5,
		TopK:          20, // отлично от дефолта bridge (40)
		MinP:          0.05,
		TypicalP:      0.9,
		TfsZ:          0.8,
		RepeatPenalty: 1.2,
		RepeatLastN:   128,
		Mirostat:      2,
		MirostatTau:   4.5,
		MirostatEta:   0.05,
		StopSequences: []string{"</s>"},
	}
	got := unsupportedBatchedSamplerParams(params)
	sort.Strings(got)

	want := []string{
		"min_p", "mirostat", "mirostat_eta", "mirostat_tau",
		"repeat_last_n", "repeat_penalty", "stop/antiprompts",
		"tfs_z", "top_k", "top_p", "typical_p",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ignored params =\n  %v\nwant\n  %v", got, want)
	}
}

// TestR65d_UnsupportedBatchedSamplerParams_AntipromptsAlone — одни только
// стоп-последовательности (частый случай: клиент ставит stop) тоже должны
// попадать в предупреждение: batched-режим их не применяет, поэтому модель
// может «не остановиться» там, где клиент ожидал.
func TestR65d_UnsupportedBatchedSamplerParams_AntipromptsAlone(t *testing.T) {
	got := unsupportedBatchedSamplerParams(BatchedSessionParams{
		Antiprompts: []string{"<|im_end|>"},
	})
	if len(got) != 1 || got[0] != "stop/antiprompts" {
		t.Errorf("Antiprompts не детектированы: %v", got)
	}

	got2 := unsupportedBatchedSamplerParams(BatchedSessionParams{
		StopSequences: []string{"</s>"},
	})
	if len(got2) != 1 || got2[0] != "stop/antiprompts" {
		t.Errorf("StopSequences не детектированы: %v", got2)
	}
}
