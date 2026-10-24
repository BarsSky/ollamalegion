// cuda_arch_guard.go — R135 (2026-10-16): образ собран под ОДНИ архитектуры CUDA,
// а карта может быть другой. Тогда первое же вычисление на GPU убивает процесс.
//
// ЖИВОЙ ДЕФЕКТ (лог бэкенда с Quadro RTX 4000, compute capability 7.5, 2026-10-16):
//
//	12:08:46 llama_prepare_model_devices: using device CUDA0 (Quadro RTX 4000)
//	12:08:47 [bridge] model loaded successfully (effective n_ctx=128000)
//	12:08:48 CUDA error: no kernel image is available for execution on the device
//	12:08:48 /build/llama.cpp/ggml/src/ggml-cuda/ggml-cuda.cu:106: CUDA error
//	12:08:48 SIGABRT: abort
//
// и так ШЕСТЬ раз подряд (12:08, 12:17, 12:28, 12:32, 12:38, 12:40). Модель при
// этом честно загружалась — падало ПЕРВОЕ ЯДРО, а не загрузка весов, поэтому
// картина для оператора выглядела необъяснимо: «загрузка пошла, модель
// загрузилась», затем бэкенд пропал («backend недоступен»), контейнер
// перезапустился, модель выгрузилась, и следующая загрузка шла уже с другими
// параметрами (профиль/запрос/дефолты) — ровно жалоба «после пошла перезагрузка
// с совсем другими параметрами».
//
// ПОЧЕМУ ЭТО НЕ ЛОВИЛОСЬ РАНЬШЕ. Никто не сравнивал архитектуры: NVML отдаёт
// compute capability карты, но образ не сообщал, под какие архитектуры собран. В
// результате единственным «детектором» был SIGABRT в C-мосте, который нельзя ни
// поймать, ни объяснить оператору.
//
// ЧТО ДЕЛАЕТ ГАРД. Пустая и дешёвая проверка ДО загрузки моделей: если известны
// и архитектуры образа, и возможности карты, и они не пересекаются — пишем
// громкую ошибку с инструкцией, что именно пересобрать. Автоматического перевода
// на CPU здесь СОЗНАТЕЛЬНО нет: это изменение поведения загрузки, и принимать его
// должен оператор (и, если включать, то отдельным решением, а не побочным
// эффектом проверки).
package cppbackend

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// EnvCUDAArchBuilt — под какие архитектуры собран образ (`CUDA_ARCH` сборки).
//
// Значение пробрасывается из build-arg в runtime-стадию Dockerfile.gpu:
// `ENV CPPWORKER_CUDA_ARCH_BUILT=${CUDA_ARCH}`. Пусто = неизвестно (старые
// образы) → проверка не выполняется и ничего не утверждается.
const EnvCUDAArchBuilt = "CPPWORKER_CUDA_ARCH_BUILT"

// BuiltCUDAArches — архитектуры, под которые собран образ: ["75","86","120"].
// Пустой срез означает «неизвестно или all» — проверять нечего.
func BuiltCUDAArches() []string {
	raw := strings.TrimSpace(os.Getenv(EnvCUDAArchBuilt))
	if raw == "" {
		return nil
	}
	lower := strings.ToLower(raw)
	if lower == "all" || strings.Contains(lower, "arch_all") {
		// Сборка под все поддерживаемые архитектуры: несовместимость невозможна.
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ';' || r == ',' || r == ' ' || r == '-'
	})
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(p), "sm_"))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// CUDAArchOfCapability — «75» для compute capability 7.5, «120» для 12.0.
func CUDAArchOfCapability(major, minor int) string {
	if major <= 0 {
		return ""
	}
	if minor < 0 {
		minor = 0
	}
	return strconv.Itoa(major) + strconv.Itoa(minor)
}

// CheckCUDAArchCompat — совместим ли образ с картой.
//
// Возвращает ok=true, если проверка пройдена ИЛИ её нельзя выполнить (нет данных,
// сборка под все архитектуры). Иначе — готовый текст ошибки, где названы: карта,
// её архитектура, архитектуры образа и что делать.
func CheckCUDAArchCompat(built []string, deviceName string, major, minor int) (bool, string) {
	if len(built) == 0 {
		return true, ""
	}
	need := CUDAArchOfCapability(major, minor)
	if need == "" {
		return true, ""
	}
	for _, b := range built {
		if b == need {
			return true, ""
		}
	}
	name := strings.TrimSpace(deviceName)
	if name == "" {
		name = "GPU"
	}
	// Архитектуры в тексте показываем как sm_86/sm_120 (привычная оператору
	// запись), а в инструкции по сборке — как значение CUDA_ARCH (86;120).
	smNames := make([]string, 0, len(built))
	for _, b := range built {
		smNames = append(smNames, "sm_"+b)
	}
	withFix := append(append([]string{}, built...), need)
	sort.Strings(withFix)
	return false, fmt.Sprintf(
		"CUDA-ядра образа не подходят этой карте: %s имеет compute capability %d.%d (sm_%s), "+
			"а образ собран под %s. Загрузка модели начнётся и даже завершится, но ПЕРВОЕ "+
			"вычисление на GPU вызовет «CUDA error: no kernel image is available for execution "+
			"on the device» и SIGABRT — процесс cppworker погибнет вместе с загруженной моделью "+
			"(снаружи это выглядит как «backend недоступен», а следующая загрузка идёт уже с "+
			"другими параметрами). Пересоберите образ с этой архитектурой: "+
			"CUDA_ARCH=%s (или CUDA_ARCH=%s, чтобы сохранить прежние карты) — "+
			"см. scripts/build-containers.ps1 -CudaArch",
		name, major, minor, need, strings.Join(smNames, ", "),
		need, strings.Join(withFix, ";"))
}

// CUDAArchWarning — предупреждение о несовпадении арок (пусто = всё в порядке).
//
// Отдаётся наружу (cppworker → /api/info → балансер/WebUI), чтобы оператор видел
// причину ДО первой загрузки: иначе она проявляется только как «backend
// недоступен» после SIGABRT, и связать это с архитектурой образа почти нельзя.
func (b *Backend) CUDAArchWarning() string {
	if b == nil {
		return ""
	}
	return b.cudaArchWarning
}

// LogCUDAArchCompat — проверка с записью в лог при старте backend'а.
//
// Возвращает текст ошибки (пусто = всё в порядке) — его можно отдать наружу
// (API/UI), чтобы оператор увидел причину до первой загрузки модели, а не в
// виде «backend недоступен» после падения.
func (b *Backend) LogCUDAArchCompat() string {
	if b == nil || len(b.gpuDevices) == 0 {
		return ""
	}
	built := BuiltCUDAArches()
	if len(built) == 0 {
		return ""
	}
	var problems []string
	for i := range b.gpuDevices {
		dev := b.gpuDevices[i]
		ok, msg := CheckCUDAArchCompat(built, dev.Name, dev.ComputeCapMajor, dev.ComputeCapMinor)
		if ok {
			continue
		}
		problems = append(problems, msg)
		if logger.Get() != nil {
			logger.Get().Errorw("CUDA-архитектуры образа и карты не совпадают — GPU-вычисления уронят процесс",
				"gpu_index", dev.Index, "gpu", dev.Name,
				"device_sm", CUDAArchOfCapability(dev.ComputeCapMajor, dev.ComputeCapMinor),
				"image_sm", strings.Join(built, ";"),
				"hint", "пересоберите образ с CUDA_ARCH, включающим архитектуру карты")
		}
	}
	if len(problems) == 0 {
		if logger.Get() != nil {
			logger.Get().Infow("CUDA-архитектуры: образ и карты совместимы", "image_sm", strings.Join(built, ";"))
		}
		return ""
	}
	return strings.Join(problems, "\n")
}
