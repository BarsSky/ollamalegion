package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// collectOllamaRuntimeFlags - сбор флагов запуска процесса Ollama
func (a *Agent) collectOllamaRuntimeFlags() types.OllamaRuntimeFlags {
	flags := types.OllamaRuntimeFlags{
		NumGPULayers:  -1, // -1 = авто (по умолчанию)
		ContextLength: 2048,
		NumParallel:   1,
		NumThreads:    0, // 0 = авто
		BatchSize:     512,
		F16KV:         true,
		Source:        "default",
	}

	// Попытка 1: аргументы процесса
	args := a.getOllamaProcessArgs()
	if len(args) > 0 {
		flags = a.parseOllamaArgs(args, flags)
		flags.Source = "process-args"
		return flags
	}

	// Попытка 2: переменные окружения OLLAMA_*
	flags = a.parseOllamaEnv(flags)
	if flags.Source != "default" {
		return flags
	}

	return flags
}

// getOllamaProcessArgs - получение аргументов процесса Ollama
func (a *Agent) getOllamaProcessArgs() []string {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		// Windows: PowerShell Get-Process
		cmd = exec.Command("powershell", "-Command",
			"Get-Process ollama -ErrorAction SilentlyContinue | Select-Object -ExpandProperty CommandLine")
	case "darwin", "linux":
		// Unix: ps aux + grep
		cmd = exec.Command("sh", "-c", "ps aux | grep -E '[o]llama serve|[o]llama run' | grep -v grep")
	default:
		return nil
	}

	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	line := strings.TrimSpace(string(output))
	if line == "" {
		return nil
	}

	// Извлекаем аргументы после "ollama"
	parts := strings.Fields(line)
	for i, p := range parts {
		if p == "ollama" || strings.HasSuffix(p, "ollama") {
			if i+1 < len(parts) {
				return parts[i+1:]
			}
		}
	}

	return nil
}

// parseOllamaArgs - парсинг аргументов командной строки Ollama
func (a *Agent) parseOllamaArgs(args []string, flags types.OllamaRuntimeFlags) types.OllamaRuntimeFlags {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		nextVal := func() string {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}

		switch arg {
		case "-ngl", "--num-gpu-layers", "--ngl":
			if v := nextVal(); v != "" {
				flags.NumGPULayers, _ = strconv.Atoi(v)
				i++
			}
		case "-c", "--ctx-size", "--context-length":
			if v := nextVal(); v != "" {
				flags.ContextLength, _ = strconv.Atoi(v)
				i++
			}
		case "-np", "--parallel":
			if v := nextVal(); v != "" {
				flags.NumParallel, _ = strconv.Atoi(v)
				i++
			}
		case "-t", "--threads":
			if v := nextVal(); v != "" {
				flags.NumThreads, _ = strconv.Atoi(v)
				i++
			}
		case "-b", "--batch-size":
			if v := nextVal(); v != "" {
				flags.BatchSize, _ = strconv.Atoi(v)
				i++
			}
		case "--split-mode":
			if v := nextVal(); v != "" {
				flags.GPUSplitMode = v
				i++
			}
		case "--main-gpu":
			if v := nextVal(); v != "" {
				flags.MainGPU, _ = strconv.Atoi(v)
				i++
			}
		case "--low-vram":
			flags.LowVRAM = true
		case "--no-kv-offload":
			flags.F16KV = false
		case "--cache-type-k":
			if v := nextVal(); v != "" {
				flags.KVCacheQuant = v
				i++
			}
		case "--flash-attn":
			flags.FlashAttention = true
		}
	}

	return flags
}

// parseOllamaEnv - чтение переменных окружения Ollama
func (a *Agent) parseOllamaEnv(flags types.OllamaRuntimeFlags) types.OllamaRuntimeFlags {
	envMap := map[string]*int{
		"OLLAMA_NUM_GPU":        &flags.NumGPULayers,
		"OLLAMA_CONTEXT_LENGTH": &flags.ContextLength,
		"OLLAMA_NUM_PARALLEL":   &flags.NumParallel,
		"OLLAMA_NUM_THREADS":    &flags.NumThreads,
	}

	found := false
	for envKey, ptr := range envMap {
		if val := os.Getenv(envKey); val != "" {
			if v, err := strconv.Atoi(val); err == nil {
				*ptr = v
				found = true
			}
		}
	}

	if val := os.Getenv("OLLAMA_KV_CACHE_TYPE"); val != "" {
		flags.KVCacheQuant = val
		found = true
	}

	if found {
		flags.Source = "env"
	}

	return flags
}

// collectModelContextInfo - сбор информации о контексте для каждой модели
func (a *Agent) collectModelContextInfo(runningModels []types.RunningModel, flags types.OllamaRuntimeFlags) []types.ModelContextInfo {
	contexts := make([]types.ModelContextInfo, 0, len(runningModels))

	for _, model := range runningModels {
		ctx := a.getModelContext(model.Name, flags)
		ctx.Name = model.Name
		ctx.ModelMemoryMB = model.VRAMUsage
		if a.platformMode == types.ModeCPU {
			ctx.ModelMemoryMB = model.RAMUsage
		}
		ctx.TotalMemoryMB = ctx.ModelMemoryMB + ctx.ContextMemoryMB
		contexts = append(contexts, ctx)
	}

	return contexts
}

// getModelContext - получение контекста модели из /api/show
func (a *Agent) getModelContext(modelName string, flags types.OllamaRuntimeFlags) types.ModelContextInfo {
	ctx := types.ModelContextInfo{
		ContextLength: 2048,
		ContextSource: "default",
		EffectiveContext: flags.ContextLength,
		PrecisionBits:  16,
		NumLayers:      32,   // fallback
		HiddenSize:     4096, // fallback
	}

	// Если F16KV выключен — используем 32 бита
	if !flags.F16KV {
		ctx.PrecisionBits = 32
	}

	// Запрос /api/show для получения параметров модели
	resp, err := a.httpClient.Get(fmt.Sprintf("%s/api/show?name=%s", a.getOllamaBaseURL(), modelName))
	if err != nil {
		// Fallback: используем env/context-length
		ctx.ContextLength = a.getContextFromEnv()
		if ctx.ContextLength != 2048 {
			ctx.ContextSource = "env"
		}
		ctx.EffectiveContext = max(ctx.ContextLength, flags.ContextLength)
		ctx.ContextMemoryMB = a.calculateContextMemory(ctx)
		ctx.KVCacheMemoryMB = a.calculateKVCacheMemory(ctx)
		return ctx
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Fallback
		ctx.ContextLength = a.getContextFromEnv()
		ctx.EffectiveContext = max(ctx.ContextLength, flags.ContextLength)
		ctx.ContextMemoryMB = a.calculateContextMemory(ctx)
		ctx.KVCacheMemoryMB = a.calculateKVCacheMemory(ctx)
		return ctx
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		ctx.ContextMemoryMB = a.calculateContextMemory(ctx)
		ctx.KVCacheMemoryMB = a.calculateKVCacheMemory(ctx)
		return ctx
	}

	var showResp struct {
		Parameters  string                 `json:"parameters"`
		ModelInfo   map[string]interface{} `json:"model_info"`
		Details     struct {
			Family        string `json:"family"`
			ParameterSize string `json:"parameter_size"`
			Quantization  string `json:"quantization_level"`
		} `json:"details"`
	}

	if err := json.Unmarshal(body, &showResp); err != nil {
		ctx.ContextMemoryMB = a.calculateContextMemory(ctx)
		ctx.KVCacheMemoryMB = a.calculateKVCacheMemory(ctx)
		return ctx
	}

	// Парсинг num_ctx из parameters
	if numCtx := parseNumCtx(showResp.Parameters); numCtx > 0 {
		ctx.ContextLength = numCtx
		ctx.ContextSource = "modelfile"
	} else {
		// Fallback на env
		ctx.ContextLength = a.getContextFromEnv()
		if ctx.ContextLength != 2048 {
			ctx.ContextSource = "env"
		}
	}

	// Извлечение архитектуры из model_info
	if arch, ok := showResp.ModelInfo["general.architecture"].(string); ok {
		ctx.NumLayers = getLayerCount(arch, showResp.Details.ParameterSize)
		ctx.HiddenSize = getHiddenSize(arch, showResp.Details.ParameterSize)
	}

	// Effective context — максимум из модели и флагов
	ctx.EffectiveContext = max(ctx.ContextLength, flags.ContextLength)
	if flags.ContextLength > ctx.ContextLength {
		ctx.ContextSource = "runtime"
	}

	// Расчет памяти
	ctx.ContextMemoryMB = a.calculateContextMemory(ctx)
	ctx.KVCacheMemoryMB = a.calculateKVCacheMemory(ctx)

	return ctx
}

// getContextFromEnv - получение контекста из переменных окружения
func (a *Agent) getContextFromEnv() int {
	if val := os.Getenv("OLLAMA_CONTEXT_LENGTH"); val != "" {
		if v, err := strconv.Atoi(val); err == nil && v > 0 {
			return v
		}
	}
	return 2048
}

// parseNumCtx - парсинг num_ctx из строки параметров
func parseNumCtx(params string) int {
	// Формат: "num_ctx 8192" или "num_ctx 4096\nstop ..."
	re := regexp.MustCompile(`num_ctx\s+(\d+)`)
	matches := re.FindStringSubmatch(params)
	if len(matches) >= 2 {
		if v, err := strconv.Atoi(matches[1]); err == nil {
			return v
		}
	}
	return 0
}

// calculateContextMemory - расчет памяти контекста
// Формула: batch_size × context_length × hidden_size × precision / 8 / 1024 / 1024
func (a *Agent) calculateContextMemory(ctx types.ModelContextInfo) uint64 {
	batchSize := 512 // fallback
	if a.currentFlags.BatchSize > 0 {
		batchSize = a.currentFlags.BatchSize
	}

	// Context overhead = batch_size × effective_context × hidden_size × precision_bits / 8
	bytes := uint64(batchSize) * uint64(ctx.EffectiveContext) * uint64(ctx.HiddenSize) * uint64(ctx.PrecisionBits) / 8
	return bytes / 1024 / 1024 // MB
}

// calculateKVCacheMemory - расчет памяти KV cache
// Формула: num_layers × 2 (K+V) × hidden_size × context_length × precision / 8 / 1024 / 1024
func (a *Agent) calculateKVCacheMemory(ctx types.ModelContextInfo) uint64 {
	bytes := uint64(ctx.NumLayers) * 2 * uint64(ctx.HiddenSize) * uint64(ctx.EffectiveContext) * uint64(ctx.PrecisionBits) / 8
	return bytes / 1024 / 1024 // MB
}

// calculateBackendCapacity - оценка ёмкости бэкенда
func (a *Agent) calculateBackendCapacity(runningModels []types.RunningModel, availableModels []types.RunningModel, flags types.OllamaRuntimeFlags) types.BackendCapacity {
	capacity := types.BackendCapacity{
		Mode: a.platformMode,
	}

	// Определяем доступную память
	var freeMemory uint64
	if a.platformMode == types.ModeGPU {
		a.mu.Lock()
		if a.currentMetrics != nil {
			capacity.FreeVRAM = a.currentMetrics.GPU.MemoryFree
			freeMemory = capacity.FreeVRAM
			// Считаем VRAM загруженных моделей
			for _, m := range runningModels {
				capacity.LoadedModelVRAM += m.VRAMUsage
			}
		}
		a.mu.Unlock()
	} else {
		a.mu.Lock()
		if a.currentMetrics != nil {
			capacity.FreeVRAM = a.currentMetrics.System.MemoryFree
			freeMemory = capacity.FreeVRAM
			for _, m := range runningModels {
				capacity.LoadedModelVRAM += m.RAMUsage
			}
		}
		a.mu.Unlock()
	}

	// Guaranteed VRAM = 90% от свободной
	capacity.GuaranteedVRAM = freeMemory * 9 / 10

	// Считаем память контекстов
	contexts := a.collectModelContextInfo(runningModels, flags)
	var totalContextMemory uint64
	for _, ctx := range contexts {
		totalContextMemory += ctx.ContextMemoryMB
	}
	capacity.ContextOverheadMB = totalContextMemory

	// Оцениваем каждую доступную модель
	capacity.AvailableModels = make([]types.AvailableModel, 0, len(availableModels))
	for _, model := range availableModels {
		am := types.AvailableModel{
			Name:          model.Name,
			Size:          model.Size,
			Family:        model.Family,
			ParameterSize: model.ParameterSize,
			Quantization:  model.Quantization,
		}

		// Размер модели в MB
		modelSizeMB := model.Size / 1024 / 1024

		// Контекст модели
		ctx := a.getModelContext(model.Name, flags)
		am.ContextLength = ctx.EffectiveContext

		// Оценка total VRAM с контекстом
		contextMem := a.calculateContextMemory(ctx) + a.calculateKVCacheMemory(ctx)
		am.EstimatedVRAM = modelSizeMB + contextMem

		// Учитываем уже используемую память
		am.VRAMUsage = capacity.LoadedModelVRAM + am.EstimatedVRAM

		// Можем ли загрузить?
		if am.EstimatedVRAM <= capacity.GuaranteedVRAM {
			am.CanLoad = true
			capacity.LoadableModelCount++
		} else {
			am.CanLoad = false
			am.LoadReason = fmt.Sprintf("requires %d MB, only %d MB guaranteed available", am.EstimatedVRAM, capacity.GuaranteedVRAM)
		}

		capacity.AvailableModels = append(capacity.AvailableModels, am)
	}

	return capacity
}

// getLayerCount - оценка количества слоёв по архитектуре и размеру
func getLayerCount(arch, paramSize string) int {
	// Извлекаем размер параметров (7B, 13B, 70B...)
	re := regexp.MustCompile(`(\d+(\.\d+)?)`)
	matches := re.FindStringSubmatch(paramSize)
	size := 7.0
	if len(matches) > 0 {
		if v, err := strconv.ParseFloat(matches[0], 64); err == nil {
			size = v
		}
	}

	switch arch {
	case "llama":
		if size >= 70 {
			return 80
		} else if size >= 30 {
			return 60
		} else if size >= 13 {
			return 40
		}
		return 32
	case "qwen2":
		if size >= 72 {
			return 80
		} else if size >= 32 {
			return 64
		} else if size >= 14 {
			return 48
		}
		return 24
	case "mistral", "mixtral":
		if size >= 8 {
			return 32
		}
		return 32
	case "phi":
		if size >= 3 {
			return 40
		}
		return 32
	default:
		if size >= 70 {
			return 80
		} else if size >= 30 {
			return 60
		} else if size >= 13 {
			return 40
		}
		return 32
	}
}

// getHiddenSize - оценка размера скрытого слоя
func getHiddenSize(arch, paramSize string) int {
	re := regexp.MustCompile(`(\d+(\.\d+)?)`)
	matches := re.FindStringSubmatch(paramSize)
	size := 7.0
	if len(matches) > 0 {
		if v, err := strconv.ParseFloat(matches[0], 64); err == nil {
			size = v
		}
	}

	switch arch {
	case "llama":
		if size >= 70 {
			return 8192
		} else if size >= 30 {
			return 6656
		} else if size >= 13 {
			return 5120
		}
		return 4096
	case "qwen2":
		if size >= 72 {
			return 8192
		} else if size >= 32 {
			return 6656
		} else if size >= 14 {
			return 5120
		}
		return 4096
	case "mistral", "mixtral":
		if size >= 8 {
			return 4096
		}
		return 4096
	case "phi":
		if size >= 3 {
			return 4096
		}
		return 2048
	default:
		if size >= 70 {
			return 8192
		} else if size >= 13 {
			return 5120
		}
		return 4096
	}
}