// image_resources_fetch.go — R633 (2026-11-19): опрос моделей и VRAM: fetchModels и обогащение VRAM из capabilities.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_resources.go` держал 2001 строку: типы и разбор
// метрик воркера, жизненный цикл опроса, гейт перед генерацией, оценка VRAM
// и замки GPU по хостам. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// fetchModels — GET /api/image/models (+ опционально VRAM из capabilities).
//
// proxyRequestToBackend переиспользуется сознательно: он уже знает, что порт
// image-бэкенда берётся из EffectiveImagePort(), и не даёт дублировать
// построение URL/таймаута.
func (r *imageResources) fetchModels(ctx context.Context, backendID string) (*imageBackendMetrics, error) {
	if r.proxy == nil {
		return nil, fmt.Errorf("image resources: proxy is nil")
	}
	// ВАЖНО: URL здесь — ОТНОСИТЕЛЬНЫЙ путь. proxyRequestToBackend сам
	// подставляет host:port бэкенда (EffectiveImagePort) и склеивает
	// `http://host:port` + r.URL.String(); абсолютный URL дал бы мусор вида
	// `http://127.0.0.1:18093http://image-backend/api/image/models`.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageModelsPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.proxy.proxyRequestToBackend(req, backendID, r.probeTimeout())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 404/405 = воркер контракта не знает («голый» sd-server на этом порту).
	// Это НЕ ошибка гейта: данных нет — значит, гейт молчит (fail-open).
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return &imageBackendMetrics{
			at:         time.Now(),
			backendID:  backendID,
			contractOK: false,
			lastErr:    fmt.Sprintf("%s returned HTTP %d (worker contract unsupported)", imageModelsPath, resp.StatusCode),
		}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned HTTP %d", imageModelsPath, resp.StatusCode)
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxImageModelsBody))
	if readErr != nil {
		return nil, fmt.Errorf("read %s body: %w", imageModelsPath, readErr)
	}
	var wire imageModelsResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", imageModelsPath, err)
	}

	snap := &imageBackendMetrics{
		at:           time.Now(),
		backendID:    backendID,
		state:        wire.State,
		currentModel: wire.CurrentModel,
		vramFreeMB:   wire.VramFreeMB,
		vramTotalMB:  wire.VramTotalMB,
	}
	if wire.VramFreeMB > 0 {
		snap.vramSource = "worker:/api/image/models"
	}
	if wire.Models == nil {
		// Ключа models нет — контракт не подтверждён: данными не пользуемся.
		snap.lastErr = fmt.Sprintf("%s has no \"models\" key (worker contract unsupported)", imageModelsPath)
		return snap, nil
	}
	snap.contractOK = true
	snap.models = *wire.Models

	// VRAM воркера: /api/image/models её не несёт, поэтому один дополнительный
	// (best-effort) запрос к capabilities, где sdworker отдаёт nvidia-smi-снимок.
	if snap.vramFreeMB <= 0 {
		r.enrichVRAMFromCapabilities(ctx, backendID, snap)
	}
	return snap, nil
}

// enrichVRAMFromCapabilities — best-effort снимок свободной VRAM воркера.
// Любая ошибка здесь не влияет на остальные метрики (VRAM остаётся неизвестной,
// и гейт по ней не судит).
func (r *imageResources) enrichVRAMFromCapabilities(ctx context.Context, backendID string, snap *imageBackendMetrics) {
	if r == nil || r.proxy == nil || snap == nil {
		return
	}
	// Относительный путь — см. комментарий в fetchModels.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageWorkerVRAMPath, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.proxy.proxyRequestToBackend(req, backendID, r.probeTimeout())
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageModelsBody))
	if err != nil {
		return
	}
	var wire imageCapabilitiesVRAMResponse
	if err := json.Unmarshal(body, &wire); err != nil || wire.VRAM == nil || !wire.VRAM.Available {
		return
	}
	if wire.VRAM.TotalMB <= 0 {
		return
	}
	free := wire.VRAM.TotalMB - wire.VRAM.UsedMB
	if free <= 0 {
		return
	}
	snap.vramFreeMB = int(free)
	snap.vramTotalMB = int(wire.VRAM.TotalMB)
	snap.vramSource = "worker:" + imageWorkerVRAMPath
}

// ============================================================
// Гейт: решение на запрос генерации
// ============================================================
