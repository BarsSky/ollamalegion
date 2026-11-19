// image_tool_load.go — R637 (2026-11-19): загрузка image-модели перед генерацией: ensureImageModelLoaded и поля прогресса.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"time"
)

// ensureImageModelLoaded — поднять модель в VRAM и дождаться готовности.
//
// ПОЧЕМУ СИНХРОННО С ПОЛЛИНГОМ, А НЕ АСИНХРОННО. Инструмент обязан вернуть
// модели результат в том же вызове (tool-сообщение) — асинхронная загрузка
// означала бы «загружаю, вернись позже», чего tool-протокол не умеет. Поэтому
// POST load → опрос /api/image/models до state=loaded, с честным таймаутом.
//
// ВОЗВРАЩАЕТСЯ ВРЕМЯ ЗАГРУЗКИ. Оно уходит в tool-результат (loadSeconds): без
// него модель не может объяснить пользователю, почему ответ шёл минуту.
func (p *Proxy) ensureImageModelLoaded(ctx context.Context, backendID, model string) (time.Duration, error) {
	started := time.Now()
	cfg := p.imageToolSettings()

	// ВАЖНО про имя поля: контракт воркера — {"name": "<model>"}
	// (cmd/sdworker/handlers_model.go: loadRequest.Name), а НЕ "model". Ошибка
	// здесь выглядела бы как «модель не поднимается» при живом воркере.
	payload, err := json.Marshal(map[string]string{"name": model})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, imageModelLoadPath, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.proxyRequestToBackend(req, backendID, cfg.LoadTimeout)
	if err != nil {
		return 0, fmt.Errorf(i18n.T("balancer.image_tool.load_request_failed"), model, err)
	}
	raw, truncated, readErr := readImageToolResponse(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return 0, fmt.Errorf(i18n.T("balancer.image_tool.load_read_failed"), model, readErr)
	}
	if truncated {
		// R120: обрезка — своя причина, а не «воркер отказал в загрузке».
		return 0, imageToolResponseTooLargeError(imageToolResponseLimitBytes)
	}
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf(i18n.T("balancer.image_tool.load_refused"),
			model, resp.StatusCode, upstreamErrorMessage(raw, resp.StatusCode))
	}

	// Ждём готовности. Опрос идёт по снимкам (ensureFreshFor сам делает опрос
	// при протухании), а НЕ по SSE-прогрессу: состояние loaded/error — это
	// то, чем живёт VRAM-гейт, и видеть его надо там же, где принимается решение
	// о генерации. Прогресс-поток (load/progress) остаётся для WebUI.
	//
	// R88: cfg.LoadTimeout == 0 = БЕЗ капа. Терминальные состояния и без капа
	// дают внятный ответ: state=loaded, state=error (воркер сообщил причину),
	// «воркер поднял другую модель» или отмена запроса (ctx.Err()).
	var deadline time.Time
	if cfg.LoadTimeout > 0 {
		deadline = started.Add(cfg.LoadTimeout)
	}
	for {
		if err := ctx.Err(); err != nil {
			return time.Since(started), fmt.Errorf(i18n.T("balancer.image_tool.load_interrupted"), model, err)
		}
		if cfg.LoadTimeout > 0 && time.Now().After(deadline) {
			return time.Since(started), fmt.Errorf(
				i18n.T("balancer.image_tool.load_timeout"),
				model, cfg.LoadTimeout, imageStateLoading, imageToolEnvLoadTimeout)
		}

		snap := p.imageResources().refresh(ctx, backendID)
		if snap != nil {
			elapsed := time.Since(started)
			if m := snap.entryByName(model); m != nil {
				switch m.State {
				case imageStateLoaded:
					return elapsed, nil
				case imageStateError:
					return elapsed, fmt.Errorf(i18n.T("balancer.image_tool.load_failed"), model,
						firstNonEmptyStr(m.Error, i18n.T("balancer.image_tool.load_worker_error_state")))
				}
			} else if snap.loaded() != nil && snap.loaded().Name != model {
				// Воркер поднял другую модель (одна модель на процесс): подменять
				// её молча нельзя — пользователь просил конкретную.
				return elapsed, fmt.Errorf(
					i18n.T("balancer.image_tool.load_wrong_model"),
					snap.loaded().Name, model)
			}
		}

		select {
		case <-ctx.Done():
			return time.Since(started), fmt.Errorf(i18n.T("balancer.image_tool.load_interrupted"), model, ctx.Err())
		case <-time.After(imageToolLoadPollInterval):
		}
	}
}

// entryByName — запись модели по имени (nil, если воркер её не сообщает).
func (s *imageBackendMetrics) entryByName(name string) *imageModelEntry {
	if s == nil {
		return nil
	}
	for i := range s.models {
		if s.models[i].Name == name {
			return &s.models[i]
		}
	}
	return nil
}

// imageToolLoadFields — поля «эту картинку сделала только что поднятая модель»
// для tool-результата.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ (живой флейк 2026-10-16). Раньше право на поля
// решалось значением: `if loadSeconds > 0`. На быстрой загрузке (стенд, уже
// прогретый диск) операция укладывается в один тик монотонных часов, и
// time.Since даёт РОВНО 0 — тогда оба поля молча пропадали, и результат
// утверждал «загрузки не было» (loadedNow=false), хотя POST load уходил и
// модель поднималась. Модель-читатель на этом основании врала пользователю:
// объяснить задержку было нечем.
//
// Теперь присутствие полей определяется ФАКТОМ загрузки (loadedNow), а значение
// не бывает нулевым: всё, что быстрее разрешения измерения, отдаём как 0.001 с
// («меньше миллисекунды»), а не как 0, который читался бы как «мгновенно».
func imageToolLoadFields(loadedNow bool, elapsed time.Duration) map[string]interface{} {
	if !loadedNow {
		return nil
	}
	ms := elapsed.Milliseconds()
	if ms <= 0 {
		ms = 1
	}
	return map[string]interface{}{
		"loadSeconds": float64(ms) / 1000,
		"loadedNow":   true,
	}
}
