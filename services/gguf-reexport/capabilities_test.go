// capabilities_test.go — честность /api/reexport/capabilities.
//
// ЗАЧЕМ. Этот ответ решает, покажет ли WebUI кнопку «полный ре-экспорт». Если
// сервис объявит `hf_to_gguf` доступной в образе без torch, кнопка будет врать:
// нажатие даст `ModuleNotFoundError: torch` вместо объяснения. Поэтому проверяется
// не «ответ непустой», а сам вердикт: без torch — available=false и причина, с
// torch — available=true.
package main

import (
	"strings"
	"testing"
)

// targetSpecByID — цель из ответа capabilities.
func targetSpecByID(caps capabilitiesResponse, id string) (targetSpec, bool) {
	for _, s := range caps.Targets {
		if s.ID == id {
			return s, true
		}
	}
	return targetSpec{}, false
}

func TestCapabilitiesHonestWithoutTorch(t *testing.T) {
	defer targetRegistry.setAvailability("hf_to_gguf", false, "образ без torch")
	defer targetRegistry.setAvailability("inkling2_loop", true, "")

	info := pythonInfo{OK: true, PythonVersion: "3.12.4", PatchScript: "scripts/gguf_loop_meta_patch.py"}
	applyAvailability(info)
	caps := buildCapabilities(info)

	spec, ok := targetSpecByID(caps, "hf_to_gguf")
	if !ok {
		t.Fatal("цель hf_to_gguf отсутствует в capabilities")
	}
	if spec.Available {
		t.Fatal("hf_to_gguf объявлена доступной в образе без torch: кнопка врала бы")
	}
	if !strings.Contains(spec.Reason, "torch") {
		t.Fatalf("причина отказа не называет torch: %q", spec.Reason)
	}
	if caps.Converter.TorchState != "absent" {
		t.Fatalf("torch_state=%q, ожидался absent", caps.Converter.TorchState)
	}
	// Метаданные и переупаковка без torch работают — их кнопка обязана остаться.
	patch, _ := targetSpecByID(caps, "inkling2_loop")
	if !patch.Available {
		t.Fatalf("inkling2_loop недоступна без torch, хотя python и скрипт патча на месте: %q", patch.Reason)
	}
}

func TestCapabilitiesHonestWithTorch(t *testing.T) {
	defer targetRegistry.setAvailability("hf_to_gguf", false, "образ без torch")

	info := pythonInfo{OK: true, PythonVersion: "3.12.4", TorchVersion: "2.4.1+cpu",
		ConverterPresent: true, PatchScript: "scripts/gguf_loop_meta_patch.py"}
	applyAvailability(info)
	caps := buildCapabilities(info)

	spec, _ := targetSpecByID(caps, "hf_to_gguf")
	if !spec.Available {
		t.Fatalf("torch и конвертер на месте, а цель недоступна: %q", spec.Reason)
	}
	if caps.Converter.TorchState != "present" {
		t.Fatalf("torch_state=%q, ожидался present", caps.Converter.TorchState)
	}

	// Конвертера нет — цель обязана закрыться даже при наличии torch.
	applyAvailability(pythonInfo{OK: true, PythonVersion: "3.12.4", TorchVersion: "2.4.1+cpu",
		ConverterPresent: false, PatchScript: "scripts/gguf_loop_meta_patch.py"})
	spec, _ = targetSpecByID(buildCapabilities(pythonInfo{}), "hf_to_gguf")
	if spec.Available {
		t.Fatal("цель доступна без convert_hf_to_gguf.py")
	}
	if !strings.Contains(spec.Reason, "convert_hf_to_gguf.py") {
		t.Fatalf("причина не называет отсутствующий конвертер: %q", spec.Reason)
	}
}

// TestCapabilitiesReportActionableProbeError — если python-часть не ответила,
// причина обязана быть в ответе, а не только в логах контейнера.
func TestCapabilitiesReportActionableProbeError(t *testing.T) {
	defer targetRegistry.setAvailability("hf_to_gguf", false, "образ без torch")
	defer targetRegistry.setAvailability("inkling2_loop", true, "")

	info := pythonInfo{OK: false, Error: "python-драйвер не ответил: exit status 2"}
	applyAvailability(info)
	caps := buildCapabilities(info)

	spec, _ := targetSpecByID(caps, "hf_to_gguf")
	if spec.Available || !strings.Contains(spec.Reason, "python-часть образа недоступна") {
		t.Fatalf("сломанная python-часть не объяснена: available=%v reason=%q", spec.Available, spec.Reason)
	}
	if caps.Converter.TorchState != "unknown" {
		t.Fatalf("torch_state=%q, ожидался unknown (пробу не выполнить)", caps.Converter.TorchState)
	}
}
