package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// desiredOllamaFlags — формирует OllamaRuntimeFlags из кэша текущих флагов
func (a *Agent) desiredOllamaFlags() types.OllamaRuntimeFlags {
	return a.currentFlags
}

// applyOllamaConfig — применяет желаемую конфигурацию Ollama через переменные окружения и перезапуск процесса
func (a *Agent) applyOllamaConfig(flags types.OllamaRuntimeFlags) error {
	// 1. Записываем флаги как переменные окружения
	envUpdates := map[string]string{
		"OLLAMA_NUM_PARALLEL":      fmt.Sprintf("%d", flags.NumParallel),
		"OLLAMA_MAX_LOADED_MODELS": fmt.Sprintf("%d", flags.MaxLoadedModels),
		"OLLAMA_KV_CACHE_TYPE":     flags.KVCacheQuant,
		"OLLAMA_NUM_THREADS":       fmt.Sprintf("%d", flags.NumThreads),
	}

	if flags.NumGPULayers >= 0 {
		envUpdates["OLLAMA_GPU_LAYERS"] = fmt.Sprintf("%d", flags.NumGPULayers)
	}
	if flags.ContextLength > 0 {
		envUpdates["OLLAMA_CONTEXT_LENGTH"] = fmt.Sprintf("%d", flags.ContextLength)
	}
	if flags.FlashAttention {
		envUpdates["OLLAMA_FLASH_ATTENTION"] = "1"
	}
	if flags.BatchSize > 0 {
		envUpdates["OLLAMA_BATCH_SIZE"] = fmt.Sprintf("%d", flags.BatchSize)
	}

	// 2. Применяем переменные окружения для текущего процесса
	for k, v := range envUpdates {
		if v != "" {
			os.Setenv(k, v)
		}
	}
	fmt.Printf("[%s] Applied Ollama env vars: %+v\n", time.Now().Format(time.RFC3339), envUpdates)

	// 3. Обновляем кэш флагов
	a.currentFlags = flags

	// 4. Запускаем перезапуск Ollama сервера (graceful restart)
	return a.restartOllamaServer(flags)
}

// restartOllamaServer — инициирует перезапуск Ollama процесса
func (a *Agent) restartOllamaServer(flags types.OllamaRuntimeFlags) error {
	// Пробуем systemctl (Linux)
	if _, err := exec.LookPath("systemctl"); err == nil {
		cmd := exec.Command("systemctl", "restart", "ollama")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl restart ollama failed: %w (output: %s)", err, string(output))
		}
		fmt.Printf("[%s] Ollama restarted via systemctl\n", time.Now().Format(time.RFC3339))
		return nil
	}

	// Пробуем service (Linux)
	if _, err := exec.LookPath("service"); err == nil {
		cmd := exec.Command("service", "ollama", "restart")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("service ollama restart failed: %w (output: %s)", err, string(output))
		}
		fmt.Printf("[%s] Ollama restarted via service\n", time.Now().Format(time.RFC3339))
		return nil
	}

	// Пробуем pkill + запуск (fallback для контейнеров/ручного запуска)
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	fmt.Printf("[%s] No init system found, attempting pkill + restart for %s\n",
		time.Now().Format(time.RFC3339), hostname)

	// Убиваем текущий процесс ollama
	killCmd := exec.Command("pkill", "-f", "ollama serve")
	killOutput, killErr := killCmd.CombinedOutput()
	if killErr != nil {
		if !strings.Contains(string(killOutput), "no process found") {
			fmt.Printf("[%s] pkill ollama warning: %v (output: %s)\n",
				time.Now().Format(time.RFC3339), killErr, string(killOutput))
		}
	}

	// Запускаем ollama serve с новыми переменными окружения
	ollamaCmd := exec.Command("ollama", "serve")
	ollamaCmd.Env = os.Environ()
	if err := ollamaCmd.Start(); err != nil {
		return fmt.Errorf("failed to start ollama serve: %w", err)
	}
	fmt.Printf("[%s] Ollama serve started with PID %d\n", time.Now().Format(time.RFC3339), ollamaCmd.Process.Pid)

	go func() {
		err := ollamaCmd.Wait()
		if err != nil {
			fmt.Printf("[%s] Ollama serve exited: %v\n", time.Now().Format(time.RFC3339), err)
		}
	}()

	a.appendLog(fmt.Sprintf("Ollama server restarted with new config: ngl=%d ctx=%d parallel=%d threads=%d batch=%d maxModels=%d",
		flags.NumGPULayers, flags.ContextLength, flags.NumParallel, flags.NumThreads, flags.BatchSize, flags.MaxLoadedModels))

	return nil
}