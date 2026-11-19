// image_resources_vram.go — R633 (2026-11-19): оценка VRAM: рабочее множество, имя загруженной модели, оценка профиля, свободная VRAM и соседи по хосту.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_resources.go` держал 2001 строку: типы и разбор
// метрик воркера, жизненный цикл опроса, гейт перед генерацией, оценка VRAM
// и замки GPU по хостам. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"fmt"
	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"strings"
	"time"
)

// imageWorkingSetMB — оценка РАБОЧЕГО набора генерации, когда веса уже в VRAM.
//
// Heuristic и почему именно такая: после загрузки модели нужно место под латенты
// (масштабируются от разрешения, не от веса), VAE-декод и буферы внимания.
// 20% веса — практичный прокси для типовых 512–1024 px; нижняя граница 256 MB
// защищает мелкие модели от заведомо недостаточной проверки, верхняя 1 GB —
// от того, чтобы «рабочий набор» крупной модели превратился в тот же полный вес.
//
// Это осознанно НЕ оценка «влезут ли веса» — она делается на этапе загрузки
// модели (свободная VRAM до загрузки против оценки из профиля/воркера).
func imageWorkingSetMB(weightsMB int) int {
	ws := weightsMB / 5
	if ws < 256 {
		ws = 256
	}
	if ws > 1024 {
		ws = 1024
	}
	return ws
}

// backendIDOrEmpty — nil-safe ID снимка (для логов).
func (s *imageBackendMetrics) backendIDOrEmpty() string {
	if s == nil {
		return ""
	}
	return s.backendID
}

// loadedModelName — имя загруженной модели (пустая строка, если неизвестно).
func (r *imageResources) loadedModelName(s *imageBackendMetrics) string {
	if s == nil {
		return ""
	}
	if m := s.loaded(); m != nil {
		return m.Name
	}
	return s.currentModel
}

// ============================================================
// Оценка VRAM
// ============================================================

// resolveVRAMEstimate — оценка потребности image-модели в VRAM.
//
// Приоритет источников (заморожен в описании types.ImageVramEstimate):
// профиль балансера → число от воркера → размеры файлов bundle'а × коэффициент
// → «unknown» (RequiredMB=0).
func (r *imageResources) resolveVRAMEstimate(s *imageBackendMetrics) types.ImageVramEstimate {
	loaded := s.loaded()
	if loaded == nil {
		// Модель неизвестна — оценивать нечего. (Гейт в этом случае либо не
		// применяется, либо отказывает кодом unknown_vram_estimate.)
		return types.ImageVramEstimate{Source: "unknown", Detail: "no loaded image model reported by the worker"}
	}

	if p, ok := r.profileFor(loaded.Name); ok && p.VramEstimateMB > 0 {
		return types.ImageVramEstimate{
			RequiredMB: p.VramEstimateMB,
			Source:     "profile",
			Detail:     fmt.Sprintf("balancer profile %q: vramEstimateMb=%d", p.Name, p.VramEstimateMB),
		}
	}

	if loaded.VramEstimateMB > 0 {
		return types.ImageVramEstimate{
			RequiredMB: loaded.VramEstimateMB,
			Source:     "worker",
			Detail:     fmt.Sprintf("worker /api/image/models reported vram_estimate_mb=%d for %q", loaded.VramEstimateMB, loaded.Name),
		}
	}

	bytes := loaded.SizeBytes
	detail := fmt.Sprintf("bundle size_bytes=%d from worker", loaded.SizeBytes)
	if bytes <= 0 {
		if p, ok := r.profileFor(loaded.Name); ok {
			var sum int64
			for _, f := range p.Files {
				sum += f.SizeBytes
			}
			bytes, detail = sum, fmt.Sprintf("sum of profile file sizes=%d", sum)
		}
	}
	if bytes > 0 {
		mb := int(float64(bytes) / (1024 * 1024) * imageVramFileCoefficient)
		if mb > 0 {
			return types.ImageVramEstimate{
				RequiredMB: mb,
				Source:     "files",
				Detail:     fmt.Sprintf("%s × %.2f (weights + VAE/latent overhead)", detail, imageVramFileCoefficient),
			}
		}
	}

	return types.ImageVramEstimate{
		Source: "unknown",
		Detail: fmt.Sprintf("model %q has no vramEstimateMb and no file-size data (profile/worker)", loaded.Name),
	}
}

// freeVRAMFor — свободная VRAM хоста, если её вообще можно узнать.
//
// Порядок: воркер (число в /api/image/models или nvidia-smi-снимок в
// capabilities) → available_vram_mb текстового бэкенда того же хоста → 0
// («неизвестно»). Ноль здесь — НЕ «память кончилась»: EvaluateImageVRAM при
// freeMB <= 0 гейт пропускает.
func (r *imageResources) freeVRAMFor(host string, s *imageBackendMetrics) (int, string) {
	if s != nil && s.vramFreeMB > 0 {
		src := s.vramSource
		if src == "" {
			src = "worker"
		}
		return s.vramFreeMB, src
	}
	if free, src := r.textNeighbourFreeVRAM(host); free > 0 {
		return free, src
	}
	return 0, "unknown"
}

// textNeighbourFreeVRAM — available_vram_mb текстовых бэкендов того же хоста.
//
// ПОЧЕМУ ЭТО НЕ «ВЫДУМАННОЕ ЧИСЛО»: cppworker отдаёт в /api/models живой
// снимок VRAM (bridge.GetGPUInfo → cudaMemGetInfo, см.
// cmd/cppworker/handlers_model.go), поллер балансера кладёт его в
// llamaMetrics[].AvailableVRAMMB. На общем хосте это и есть бюджет карты.
//
// ОГРАНИЧЕНИЯ (сознательные): берётся МАКСИМУМ по бэкендам хоста (на машине с
// несколькими GPU available_vram_mb — сумма по устройствам, и минимум давал бы
// ложные отказы), плюс требование свежести снимка (maxTextVramSnapshotAge).
// Fallback на минимум/конкретное устройство — отдельная задача (нужен GPU-индекс
// в конфиге бэкенда, которого в контракте Phase 1 нет).
func (r *imageResources) textNeighbourFreeVRAM(host string) (int, string) {
	if r == nil || r.proxy == nil || host == "" {
		return 0, ""
	}
	p := r.proxy
	ids := r.textNeighbourIDs(host)
	if len(ids) == 0 {
		return 0, ""
	}

	now := time.Now()
	best, src := 0, ""
	p.metricsMgr.mu.RLock()
	for _, id := range ids {
		lm := p.metricsMgr.llamaMetrics[id]
		if lm == nil || lm.AvailableVRAMMB == 0 {
			continue
		}
		if bm := p.metricsMgr.metrics[id]; bm != nil && !bm.Timestamp.IsZero() &&
			now.Sub(bm.Timestamp) > maxTextVramSnapshotAge {
			continue
		}
		if int(lm.AvailableVRAMMB) > best {
			best = int(lm.AvailableVRAMMB)
			src = "text_neighbour:" + id
		}
	}
	p.metricsMgr.mu.RUnlock()
	return best, src
}

// hostHasTextNeighbour — есть ли на хосте текстовый (не image) бэкенд.
// Именно это делает сценарий «сосуществование» реальным.
func (r *imageResources) hostHasTextNeighbour(host string) bool {
	return len(r.textNeighbourIDs(host)) > 0
}

// textNeighbourIDs — текстовые (не image) бэкенды, которые делят с image-бэкендом
// этого хоста ОДНУ физическую карту.
//
// ДВА признака, а не один:
//
//  1. Совпадение host — прежнее правило. Годится, когда ноды адресуются IP-ом
//     (на второй машине стенда и текст, и image зарегистрированы как
//     192.0.2.11).
//  2. Совпадение GPU UUID — R91 (2026-10-09). На первой машине стенда текстовая
//     нода регистрируется как `cppworker-gpu`, а image-воркер как `imageworker`
//     (имена контейнеров), поэтому сравнение только по host НЕ находило соседа —
//     хотя обе живут на одной RTX 3070 и агенты обеих отдают один и тот же
//     `gpu.uuids: ["GPU-6f8d5f3b-..."]`. Следствие: сообщение VRAM-гейта не
//     называло виновника («нужно 520 МБ, свободно 512 МБ» без ответа на вопрос
//     «кто занял карту»), а fallback свободной VRAM не находил текстовый бюджет.
//     UUID — тот же признак, по которому монитор схлопывает VRAM двух бэкендов в
//     одну карту (webui: aggregate VRAM by UUID).
//
// Пустой список = «shared GPU не обнаружен» (нет метрик агента, у бэкендов нет
// UUID). Это НЕ повод блокировать: вызывающие трактуют пустоту как «не знаем».
func (r *imageResources) textNeighbourIDs(host string) []string {
	if r == nil || r.proxy == nil || host == "" {
		return nil
	}
	p := r.proxy

	// UUID карт, которые обслуживают image-бэкенды этого хоста.
	imageUUIDs := map[string]bool{}
	p.mu.RLock()
	for id, st := range p.backends {
		if st == nil || st.Backend == nil {
			continue
		}
		if normalizeBackendType(st.Backend.Type) != types.BackendTypeImage {
			continue
		}
		if st.Backend.Host != host {
			continue
		}
		if m, ok := p.metricsMgr.SnapshotBackendMetrics(id); ok && m != nil {
			for _, u := range m.GPU.UUIDs {
				if u != "" {
					imageUUIDs[u] = true
				}
			}
		}
	}

	ids := make([]string, 0, 2)
	for id, st := range p.backends {
		if st == nil || st.Backend == nil {
			continue
		}
		if normalizeBackendType(st.Backend.Type) == types.BackendTypeImage {
			continue
		}
		if st.Backend.Status == types.StatusOffline {
			continue
		}
		sameHost := st.Backend.Host == host
		sameGPU := false
		if !sameHost && len(imageUUIDs) > 0 {
			if m, ok := p.metricsMgr.SnapshotBackendMetrics(id); ok && m != nil {
				for _, u := range m.GPU.UUIDs {
					if u != "" && imageUUIDs[u] {
						sameGPU = true
						break
					}
				}
			}
		}
		if sameHost || sameGPU {
			ids = append(ids, id)
		}
	}
	p.mu.RUnlock()
	return ids
}

// profileFor — профиль image-модели на балансере (только чтение).
//
// Профили лежат в отдельном файле (internal/config.ImageModelProfileStore),
// потому что pkg/types/config.go — замороженный контракт Phase 1. Хранилище
// перечитывается не чаще imageProfilesRefreshInterval: оператор правит
// vramEstimateMb без рестарта балансера.
func (r *imageResources) profileFor(name string) (types.ImageModelProfile, bool) {
	if r == nil || strings.TrimSpace(name) == "" {
		return types.ImageModelProfile{}, false
	}
	store := r.profileStore()
	if store == nil {
		return types.ImageModelProfile{}, false
	}
	// Ошибку загрузки профилей НЕ глотаем: битый/нечитаемый файл — это причина,
	// по которой гейт скажет «неизвестно», и оператор обязан её увидеть
	// (иначе выглядит как «профиль есть, а гейт его не видит»).
	if err := store.EnsureLoaded(); err != nil {
		logger.Get().Warnw("image model profiles load failed: VRAM estimate falls back to worker/files",
			"path", store.Path(), "error", err)
		return types.ImageModelProfile{}, false
	}
	if p, ok := store.Get(name); ok {
		return p, true
	}
	// Регистр: воркер отдаёт имя каталога (bundle), профиль мог быть сохранён с
	// другим регистром. Это не «угадывание» — сравнение точное, но без регистра.
	lower := strings.ToLower(name)
	for n, p := range store.List() {
		if strings.ToLower(n) == lower {
			return p, true
		}
	}
	return types.ImageModelProfile{}, false
}

// profileStore — ленивый read-only доступ к хранилищу профилей image-моделей.
func (r *imageResources) profileStore() *config.ImageModelProfileStore {
	if r == nil {
		return nil
	}
	r.profileMu.Lock()
	defer r.profileMu.Unlock()
	now := time.Now()
	if r.profiles == nil {
		r.profiles = config.NewImageModelProfileStore("")
		r.profilesAt = now
		logger.Get().Infow("image model profiles store initialized (read-only)",
			"path", r.profiles.Path())
		return r.profiles
	}
	if now.Sub(r.profilesAt) > imageProfilesRefreshInterval {
		if err := r.profiles.Load(); err != nil {
			logger.Get().Warnw("image model profiles reload failed",
				"path", r.profiles.Path(), "error", err)
		}
		r.profilesAt = now
	}
	return r.profiles
}

// ============================================================
// Лок GPU: захват/освобождение/предохранитель
// ============================================================
