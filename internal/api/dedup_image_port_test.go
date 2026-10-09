package api

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// R-MultiHost (2026-10-07).
//
// Дефект на живой паре из двух машин. Image-бэкенд (image_cpp) не имеет
// Ollama-поверхности, но в записи всё равно лежит ollamaPort=11434 — дефолт,
// который подставляет agentRegisterHandler. Ключ де-дупликации получался
// (host, 11434), и на машине, где текстовый и image-бэкенд объявляют ОДИН host
// (BACKEND_HOST=192.0.2.11 сразу для обоих воркеров), ключи совпадали.
// Из двух записей оставалась одна — побеждал меньший ID (CPPWORKER-34), а
// image-бэкенд «мигал» в WebUI: появлялся, пока запись была создана
// саморегистрацией sdworker'а (ollamaPort ещё 0), и исчезал после обновления от
// встроенного агента, который проставляет ollamaPort=11434.

func backendForDedup(id, host, btype string, ollamaPort, cppPort, imagePort int, hasAgent bool) types.Backend {
	return types.Backend{
		ID:            id,
		Host:          host,
		Type:          types.BackendType(btype),
		OllamaPort:    ollamaPort,
		CppWorkerPort: cppPort,
		ImagePort:     imagePort,
		HasAgent:      hasAgent,
	}
}

func dedupBackends(list []types.Backend) []types.Backend {
	return dedupBackendsByHostPort(
		list,
		func(b types.Backend) string { return b.Host },
		backendDedupPort,
		func(b types.Backend) bool { return b.HasAgent },
		true,
	)
}

// TestBackendDedupPort_ImageUsesImagePort — ключ image-бэкенда не должен
// совпадать с ключом текстового на том же хосте.
func TestBackendDedupPort_ImageUsesImagePort(t *testing.T) {
	img := backendForDedup("IMAGEWORKER-34", "192.0.2.11", string(types.BackendTypeImage), 11434, 0, 18093, true)
	txt := backendForDedup("CPPWORKER-34", "192.0.2.11", string(types.BackendTypeLlamaCpp), 11434, 18092, 0, true)

	if got := backendDedupPort(img); got != 18093 {
		t.Fatalf("backendDedupPort(image) = %d, ожидался 18093 (порт image-воркера, а не ollamaPort=11434)", got)
	}
	if got := backendDedupPort(txt); got != 11434 {
		t.Fatalf("backendDedupPort(llama_cpp) = %d, ожидался 11434 (прежнее поведение)", got)
	}
	if backendDedupPort(img) == backendDedupPort(txt) {
		t.Fatal("ключи де-дупликации текстового и image-бэкенда на одном хосте совпали — image-бэкенд пропадёт из списка")
	}
}

// TestBackendDedupPort_ImageFallback — imagePort не задан → EffectiveImagePort
// подставляет дефолт, ключ всё равно не равен 11434 и не равен нулю.
func TestBackendDedupPort_ImageFallback(t *testing.T) {
	img := backendForDedup("imageworker", "192.0.2.11", string(types.BackendTypeImage), 11434, 0, 0, true)
	got := backendDedupPort(img)
	if got == 0 || got == 11434 {
		t.Fatalf("backendDedupPort(image без imagePort) = %d, ожидался fallback EffectiveImagePort", got)
	}
}

// TestDedup_TextAndImageOnSameHostBothVisible — оба бэкенда одной машины должны
// остаться в списке: именно их исчезновение и выглядело как «мигание».
func TestDedup_TextAndImageOnSameHostBothVisible(t *testing.T) {
	list := []types.Backend{
		backendForDedup("CPPWORKER-34", "192.0.2.11", string(types.BackendTypeLlamaCpp), 11434, 18092, 0, true),
		backendForDedup("IMAGEWORKER-34", "192.0.2.11", string(types.BackendTypeImage), 11434, 0, 18093, true),
	}
	result := dedupBackends(list)
	if len(result) != 2 {
		ids := make([]string, 0, len(result))
		for _, b := range result {
			ids = append(ids, b.ID)
		}
		t.Fatalf("после де-дупа осталось %d записей (%v), ожидалось 2: текстовый и image-бэкенд одной машины", len(result), ids)
	}
}

// TestDedup_RealDuplicateStillCollapsed — настоящий дубль (тот же host и тот же
// физический порт) по-прежнему схлопывается: фикс не должен ломать прежнюю
// защиту от повторных регистраций.
func TestDedup_RealDuplicateStillCollapsed(t *testing.T) {
	list := []types.Backend{
		backendForDedup("cppworker-gpu", "cppworker-gpu", string(types.BackendTypeLlamaCpp), 11434, 18092, 0, false),
		backendForDedup("cppworker-gpu-bundled-agent", "cppworker-gpu", string(types.BackendTypeLlamaCpp), 11434, 18092, 0, true),
	}
	result := dedupBackends(list)
	if len(result) != 1 {
		t.Fatalf("после де-дупа осталось %d записей, ожидалась 1", len(result))
	}
	if result[0].ID != "cppworker-gpu-bundled-agent" {
		t.Fatalf("оставлен %q, ожидался бэкенд с агентом", result[0].ID)
	}
}
