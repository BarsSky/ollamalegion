//go:build llama_stub

// dockerfiles_no_arch_variants_r493d_test.go — R493d (2026-11-17).
//
// ЗАЧЕМ. Живой отказ: `scripts/build-containers.ps1 -CudaArch 86` выбирал
// `docker/cppworker/Dockerfile.gpu.86` — копию, отставшую от `Dockerfile.gpu` на
// 330 строк и последний раз тронутую 2026-11-10. Сборка падала на
//
//	error: patch failed: ggml/src/ggml-rpc/transport.cpp:717
//	error: ggml/src/ggml-rpc/transport.cpp: patch does not apply
//
// то есть сообщение указывало на ПАТЧ, хотя оба патча к текущему сабмодулю
// применяются (`git apply --check` = 0), а виноват был устаревший Dockerfile,
// ссылавшийся на удалённый 0002 и не копировавший c/bridge/window_fit.c.
//
// Почему это не поймал ни один тест: сторожа R214/R226 проверяли НАЛИЧИЕ правок
// в перечисленном наборе Dockerfile'ов, но никто не проверял, что набор
// единственный. Копия «просто была ещё одним файлом в списке» — и проходила
// проверки, пока её содержимое не разошлось с действительностью.
//
// Контракт R493d:
//  1. в docker/cppworker не существует Dockerfile.gpu.<arch> — архитектура
//     задаётся build-arg'ом CUDA_ARCH одному файлу;
//  2. Dockerfile.gpu эту роль действительно выполняет (объявляет ARG CUDA_ARCH и
//     подставляет её в CMAKE_CUDA_ARCHITECTURES);
//  3. scripts/build-containers.ps1 не выбирает Dockerfile по архитектуре.
package cppbackend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestR493d_NoArchSpecificGpuDockerfile — вторая копия сборки GPU-образа не
// должна появиться снова: её никто не обновляет, и она тихо выигрывает выбор.
func TestR493d_NoArchSpecificGpuDockerfile(t *testing.T) {
	matches, err := filepath.Glob("../../docker/cppworker/Dockerfile.gpu.*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range matches {
		t.Errorf("найден %s: отдельная копия Dockerfile.gpu под архитектуру запрещена — "+
			"она отстаёт от основной и ломает сборку сообщением про патч (R493d). "+
			"Архитектура задаётся --build-arg CUDA_ARCH", filepath.Base(path))
	}
}

// TestR493d_UniversalDockerfileTakesArchFromArg — единственный файл обязан
// уметь собирать под запрошенную арку, иначе удаление копии сломало бы sm_86.
func TestR493d_UniversalDockerfileTakesArchFromArg(t *testing.T) {
	const path = "../../docker/cppworker/Dockerfile.gpu"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не читается %s: %v", path, err)
	}
	text := string(raw)
	if !strings.Contains(text, "ARG CUDA_ARCH") {
		t.Fatal("Dockerfile.gpu не объявляет ARG CUDA_ARCH — одиночную арку больше нечем задать")
	}
	if !strings.Contains(text, `GGML_CUDA_ARCH_LIST="${CUDA_ARCH}"`) {
		t.Error("Dockerfile.gpu не подставляет CUDA_ARCH в GGML_CUDA_ARCH_LIST: " +
			"сборка молча уедет на дефолтный список архитектур")
	}
}

// TestR493d_BuildScriptDoesNotPickDockerfileByArch — скрипт выпуска обязан
// указывать один и тот же файл независимо от -CudaArch.
func TestR493d_BuildScriptDoesNotPickDockerfileByArch(t *testing.T) {
	const path = "../../scripts/build-containers.ps1"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не читается %s: %v", path, err)
	}
	text := string(raw)
	if strings.Contains(text, "Dockerfile.gpu.$CudaArch") {
		t.Error("build-containers.ps1 снова выбирает Dockerfile по архитектуре: " +
			"такая копия устаревает и падает на применении патчей (R493d)")
	}
	if !strings.Contains(text, `$dockerfile = "docker/cppworker/Dockerfile.gpu"`) {
		t.Error("build-containers.ps1 не задаёт universal-путь к Dockerfile.gpu")
	}
}

// TestR493d_BuildArgsArePairedWithFlag — сторож к тому же дефекту выпуска:
// шесть значений (LLAMA_BUILD, LLAMA_BUILD_DATE, RPC_PROTO, SERVICE, VERSION,
// IMAGE_TAG) добавлялись к ОДНОМУ `--build-arg`. Docker (buildx) съедает одно
// значение, остальные становятся позиционными аргументами, и сборка падает ДО
// компиляции строкой `docker: 'docker buildx build' requires 1 argument`.
//
// Проверяем ровно тот код, который был сломан: в блоке формирования
// $gpuBuildArgs каждая строка `+= "<...>=<...>"` обязана идти после `--build-arg`.
func TestR493d_BuildArgsArePairedWithFlag(t *testing.T) {
	const path = "../../scripts/build-containers.ps1"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не читается %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")

	// Значение build-arg выглядит как `$x += "KEY=..."` (KEY — заглавные/подчёркивания).
	isValueAppend := func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "$gpuBuildArgs +=") {
			return false
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "$gpuBuildArgs +="))
		value = strings.Trim(value, `"'`)
		eq := strings.Index(value, "=")
		if eq <= 0 {
			return false
		}
		key := value[:eq]
		if strings.Contains(key, "$") || strings.Contains(key, " ") {
			return false
		}
		return key == strings.ToUpper(key)
	}

	checked := 0
	for i, line := range lines {
		if !isValueAppend(line) {
			continue
		}
		checked++
		prev := ""
		if i > 0 {
			prev = strings.TrimSpace(lines[i-1])
		}
		if prev != `$gpuBuildArgs += "--build-arg"` {
			t.Errorf("строка %d: значение build-arg добавлено без `--build-arg` перед ним:\n  %s\n"+
				"docker съест только одно значение, остальные станут позиционными аргументами "+
				"и сборка упадёт с «requires 1 argument» (R493d)", i+1, strings.TrimSpace(line))
		}
	}
	if checked == 0 {
		t.Fatal("не найдено ни одной строки со значением build-arg — сторож ничего не проверил")
	}
}
