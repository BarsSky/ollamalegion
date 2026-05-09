// Package virtualmodel — Вариант C: Virtual Model Router
// Реализует абстракцию VirtualModel, которая маппится на несколько
// физических моделей на разных бэкендах (pipeline parallelism).
package virtualmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// VirtualModel представляет виртуальную модель, состоящую из нескольких
// физических срезов (slices), распределённых по разным бэкендам.
type VirtualModel struct {
	Config     types.VirtualModelConfig
	mu         sync.RWMutex
	activeJobs map[string]*SliceExecutionContext // requestID → context
}

// SliceExecutionContext — контекст выполнения одного запроса через pipeline.
type SliceExecutionContext struct {
	RequestID    string
	VirtualModel string
	CurrentSlice int
	InputData    []byte
	Output       chan SliceResult
	Error        error
	Done         chan struct{}
}

// SliceResult — результат выполнения одного среза.
type SliceResult struct {
	SliceID string
	Data    []byte
	Error   error
}

// sliceHTTPClient — HTTP-клиент для межсрезовых вызовов.
var sliceHTTPClient = &http.Client{
	Timeout: 300 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxConnsPerHost:     100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

// NewVirtualModel создаёт новую виртуальную модель.
func NewVirtualModel(cfg types.VirtualModelConfig) *VirtualModel {
	return &VirtualModel{
		Config:     cfg,
		activeJobs: make(map[string]*SliceExecutionContext),
	}
}

// GetName возвращает имя виртуальной модели.
func (vm *VirtualModel) GetName() string {
	return vm.Config.Name
}

// ExecutePipeline выполняет полный pipeline для запроса (sequential).
// Возвращает финальный ответ от последнего среза.
func (vm *VirtualModel) ExecutePipeline(input []byte, params map[string]string, opts ...PipelineOption) ([]byte, error) {
	// Применяем опции
	po := &pipelineOptions{}
	for _, opt := range opts {
		opt(po)
	}

	requestID := po.requestID
	if requestID == "" {
		requestID = fmt.Sprintf("vm-%s-%d", vm.Config.Name, time.Now().UnixNano())
	}

	timeoutMs := vm.Config.Coordination.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 30000 // 30s default
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond

	// Сортируем срезы по ordinal
	slices := sortSlices(vm.Config.Slices)

	// Проверяем, что срезы не пусты
	if len(slices) == 0 {
		return nil, fmt.Errorf("virtual model %s has no slices", vm.Config.Name)
	}

	// Создаём контекст выполнения
	ctx := &SliceExecutionContext{
		RequestID:    requestID,
		VirtualModel: vm.Config.Name,
		CurrentSlice: 0,
		InputData:    input,
		Output:       make(chan SliceResult, len(slices)),
		Done:         make(chan struct{}),
	}

	// Регистрируем активную задачу
	vm.mu.Lock()
	vm.activeJobs[requestID] = ctx
	vm.mu.Unlock()

	// Очищаем при завершении
	defer func() {
		vm.mu.Lock()
		delete(vm.activeJobs, requestID)
		vm.mu.Unlock()
		close(ctx.Done)
	}()

	currentInput := input
	selectAllParams := vm.collectParams(params)

	// Sequential pipeline: каждый срез получает output предыдущего как input
	for i, slice := range slices {
		ctx.CurrentSlice = i

		// Выбираем целевой бэкенд для среза
		targetHost, targetPort, err := selectBackendForSlice(slice.TargetBackends, po.backendLoadFn)
		if err != nil {
			errMsg := fmt.Errorf("slice %s (%s): no available backend: %w",
				slice.ID, slice.ModelName, err)
			ctx.Error = errMsg

			if slice.FallbackMode == "skip" {
				logger.Get().Warnw("skipping slice due to no backends",
					"slice", slice.ID, "model", slice.ModelName)
				continue
			}
			return nil, errMsg
		}

		// Выполняем HTTP-вызов к бэкенду
		result, err := vm.callSliceBackend(targetHost, targetPort, slice.ModelName, currentInput, selectAllParams, timeout)
		if err != nil {
			if slice.FallbackMode == "skip" {
				logger.Get().Warnw("slice failed, skipping",
					"slice", slice.ID, "model", slice.ModelName, "error", err)
				continue
			}
			return nil, fmt.Errorf("slice %s (%s) failed: %w", slice.ID, slice.ModelName, err)
		}

		// Отправляем результат в канал для мониторинга
		ctx.Output <- SliceResult{
			SliceID: slice.ID,
			Data:    result,
		}

		currentInput = result
	}

	return currentInput, nil
}

// callSliceBackend выполняет HTTP-вызов к бэкенду для среза.
func (vm *VirtualModel) callSliceBackend(host string, port int, modelName string, input []byte, params map[string]string, timeout time.Duration) ([]byte, error) {
	targetURL := fmt.Sprintf("http://%s:%d/api/generate", host, port)

	// Разбираем входные данные как JSON
	var requestBody map[string]interface{}
	if err := json.Unmarshal(input, &requestBody); err != nil {
		// Если не JSON — используем сырые данные
		requestBody = map[string]interface{}{
			"prompt": string(input),
		}
	}

	// Устанавливаем имя модели для этого среза
	requestBody["model"] = modelName

	// Добавляем дополнительные параметры (кроме model)
	for k, v := range params {
		if k != "model" {
			requestBody[k] = v
		}
	}

	// Сериализуем тело запроса
	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Создаём HTTP-запрос (non-streaming — ждём полный ответ)
	req, err := http.NewRequest("POST", targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Выполняем запрос с таймаутом
	client := sliceHTTPClient
	if timeout > 0 {
		client = &http.Client{Timeout: timeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("backend returned status %d: %s", resp.StatusCode, string(body))
	}

	// Читаем ответ
	responseBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return responseBytes, nil
}

// collectParams собирает все параметры для передачи между срезами.
func (vm *VirtualModel) collectParams(params map[string]string) map[string]string {
	if params == nil {
		return make(map[string]string)
	}
	result := make(map[string]string, len(params))
	for k, v := range params {
		result[k] = v
	}
	return result
}

// GetActiveJobs возвращает количество активных задач.
func (vm *VirtualModel) GetActiveJobs() int {
	vm.mu.RLock()
	defer vm.mu.RUnlock()
	return len(vm.activeJobs)
}

// GetActiveJobInfos возвращает информацию о всех активных задачах.
func (vm *VirtualModel) GetActiveJobInfos() []map[string]interface{} {
	vm.mu.RLock()
	defer vm.mu.RUnlock()
	infos := make([]map[string]interface{}, 0, len(vm.activeJobs))
	for id, ctx := range vm.activeJobs {
		infos = append(infos, map[string]interface{}{
			"requestId":    id,
			"currentSlice": ctx.CurrentSlice,
			"hasError":     ctx.Error != nil,
		})
	}
	return infos
}

// ---------- Вспомогательные функции ----------

// sortSlices сортирует срезы по ordinal.
func sortSlices(slices []types.ModelSliceConfig) []types.ModelSliceConfig {
	sorted := make([]types.ModelSliceConfig, len(slices))
	copy(sorted, slices)
	// Простая сортировка вставками (срезов обычно мало, ≤10)
	for i := 1; i < len(sorted); i++ {
		key := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j].Ordinal > key.Ordinal {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = key
	}
	return sorted
}

// selectBackendForSlice выбирает бэкенд из списка targetBackends.
// Если список пуст, возвращает ошибку.
// Если задана функция оценки загрузки, выбирает наименее загруженный.
func selectBackendForSlice(targetBackends []string, loadFn func(backendID string) float64) (string, int, error) {
	if len(targetBackends) == 0 {
		return "", 0, fmt.Errorf("no target backends configured")
	}

	// Если функция оценки не задана — берём первый
	if loadFn == nil {
		return parseBackendID(targetBackends[0])
	}

	// Выбираем наименее загруженный
	var bestID string
	var bestLoad float64 = 2.0 // больше max возможной нагрузки
	for _, id := range targetBackends {
		load := loadFn(id)
		if load < bestLoad {
			bestLoad = load
			bestID = id
		}
	}

	if bestID == "" {
		bestID = targetBackends[0]
	}

	return parseBackendID(bestID)
}

// parseBackendID разбирает строку "host:port" вида "127.0.0.1:11434".
func parseBackendID(id string) (string, int, error) {
	host := id
	port := 11434

	// Ищем порт
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == ':' {
			host = id[:i]
			if n, err := fmt.Sscanf(id[i+1:], "%d", &port); err != nil || n != 1 {
				return "", 0, fmt.Errorf("invalid backend ID format: %s", id)
			}
			break
		}
	}

	return host, port, nil
}

// ---------- Pipeline Options ----------

type pipelineOptions struct {
	requestID     string
	backendLoadFn func(backendID string) float64
}

// PipelineOption — опция для ExecutePipeline.
type PipelineOption func(*pipelineOptions)

// WithRequestID устанавливает ID запроса для отслеживания.
func WithRequestID(id string) PipelineOption {
	return func(o *pipelineOptions) {
		o.requestID = id
	}
}

// WithBackendLoadFn устанавливает функцию оценки загрузки бэкендов.
func WithBackendLoadFn(fn func(backendID string) float64) PipelineOption {
	return func(o *pipelineOptions) {
		o.backendLoadFn = fn
	}
}

// GetSliceModelName возвращает имя модели для заданного среза по ID.
func (vm *VirtualModel) GetSliceModelName(sliceID string) string {
	for _, slice := range vm.Config.Slices {
		if slice.ID == sliceID {
			return slice.ModelName
		}
	}
	return ""
}

// GetSlices возвращает срезы модели.
func (vm *VirtualModel) GetSlices() []types.ModelSliceConfig {
	result := make([]types.ModelSliceConfig, len(vm.Config.Slices))
	copy(result, vm.Config.Slices)
	return result
}
