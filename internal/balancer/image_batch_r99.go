// Package balancer — R99 (S8 плана 2026-10-07): диспетчер пачки изображений.
//
// План §6 (S8): «Диспетчер пачки: `n` изображений раскладываются по всем
// image-бэкендам как независимые задачи с общим ответом». AC: «4 изображения на
// 2 бэкенда — wall-time ≈ половина одиночного (если узлы равноценны), каждое
// видно в статистике».
//
// ПОЧЕМУ ЭТО ЧЕСТНОЕ РАСПРЕДЕЛЕНИЕ, А НЕ «ЕЩЁ ОДИН РЕЖИМ». sd.cpp не умеет делить
// ОДНУ генерацию по GPU (§10 плана: stage-split — это форк/ресёрч). Но `n`
// изображений — это `n` НЕЗАВИСИМЫХ задач: их можно считать на разных картах
// параллельно и склеить ответ. Никакого нового транспорта для этого не нужно, и
// именно поэтому это единственное настоящее ускорение imageworker'а.
//
// ЧТО РАСКЛАДЫВАЕТСЯ И ЧТО НЕТ:
//   - `/v1/images/generations` (OpenAI, число картинок = `n`) — да;
//   - `/sdapi/v1/txt2img` (A1111, число картинок = `batch_size` × `n_iter`) — да;
//   - `/api/image/generate` — НЕТ: это постановка асинхронной джобы (202 + job id),
//     у неё нет «общего ответа» из картинок, и раскладывать её на несколько
//     job-id значило бы сломать контракт клиента;
//   - `/v1/images/edits` и `/variations` — НЕТ: они принимают multipart-форму и
//     иную семантику `n`; переносить их в пачку без стенда не проверить.
//
// ОТКАЗ ЧЕСТНЫЙ, А НЕ ЧАСТИЧНЫЙ. Если хотя бы одна задача пачки упала, клиент
// получает ошибку с указанием узла, а не «3 картинки вместо 4»: молча недодать
// изображения — худшее из возможного (клиент не поймёт, что что-то потерялось).
//
// Гейт VRAM и лок GPU берутся НА КАЖДЫЙ узел пачки (beforeGeneration): иначе
// раскладка обошла бы ровно ту защиту, ради которой гейт существует, и запустила
// бы генерации на занятых картах. Узел, чей гейт отказал, просто не участвует —
// пачка считается на тех, кто готов (если готовых меньше двух, работает
// обычный одиночный путь).
package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

const (
	// imageBatchBodyLimitR99 — сколько читаем из тела запроса (как у метаданных).
	imageBatchBodyLimitR99 = 1 << 20
	// imageBatchPartLimitR99 — предел на ответ одного узла пачки.
	// Синхронный ответ — base64 картинок: 4×1024×1024 PNG ≈ 4 МиБ, с запасом 64.
	imageBatchPartLimitR99 = 64 << 20

	// Стили поверхности: чем выражается число картинок.
	imageBatchStyleOpenAI = "openai" // n
	imageBatchStyleA1111  = "a1111"  // batch_size × n_iter
)

// imageBatchChunkR99 — одна независимая задача пачки.
type imageBatchChunkR99 struct {
	BackendID string
	Count     int
}

// imageBatchPlanR99 — разложить n картинок по узлам.
//
// Остаток распределяется по первым задачам (4 на 2 узла → 2+2; 5 на 2 → 3+2):
// узлов больше, чем картинок, не бывает — лишние не получают работы вовсе.
func imageBatchPlanR99(n int, backendIDs []string) []imageBatchChunkR99 {
	if n <= 0 || len(backendIDs) == 0 {
		return nil
	}
	k := len(backendIDs)
	if k > n {
		k = n
	}
	base, extra := n/k, n%k
	plan := make([]imageBatchChunkR99, 0, k)
	for i := 0; i < k; i++ {
		count := base
		if i < extra {
			count++
		}
		if count <= 0 {
			continue
		}
		plan = append(plan, imageBatchChunkR99{BackendID: backendIDs[i], Count: count})
	}
	return plan
}

// imageBatchSpecR99 — сколько картинок просит запрос и в каком стиле.
//
// ok=false означает «это не тот запрос, который стоит раскладывать» — вызывающий
// просто идёт обычным одиночным путём.
func imageBatchSpecR99(body []byte, path string) (count int, style string, ok bool) {
	switch path {
	case "/v1/images/generations":
		style = imageBatchStyleOpenAI
	case "/sdapi/v1/txt2img":
		style = imageBatchStyleA1111
	default:
		return 0, "", false
	}
	if len(body) == 0 {
		return 1, style, true // без тела просят одну картинку (дефолт обеих поверхностей)
	}

	var payload struct {
		N         int `json:"n"`
		BatchSize int `json:"batch_size"`
		NIter     int `json:"n_iter"`
		NIterAlt  int `json:"nIter"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// Тело не разобрали — не гадаем: одиночный путь вернёт клиенту
		// осмысленную ошибку движка вместо нашей выдумки.
		return 0, "", false
	}

	if style == imageBatchStyleA1111 {
		batch := payload.BatchSize
		iters := payload.NIter
		if iters <= 0 {
			iters = payload.NIterAlt
		}
		if iters > 0 {
			if batch > 0 {
				batch *= iters
			} else {
				batch = iters
			}
		}
		if batch <= 0 {
			batch = 1
		}
		return batch, style, true
	}
	if payload.N <= 0 {
		return 1, style, true
	}
	return payload.N, style, true
}

// imageBatchBodyR99 — тело запроса под ОДНУ задачу пачки (count картинок).
//
// Остальные поля сохраняются как есть (RawMessage): промпт, размеры, шаги,
// seed и всё, что движок понимает сам, — не наше дело.
func imageBatchBodyR99(body []byte, style string, count int) ([]byte, error) {
	if count <= 0 {
		count = 1
	}
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf(i18n.T("balancer.image_batch.body_not_json"), err)
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	set := func(key string, value int) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		fields[key] = raw
		return nil
	}
	switch style {
	case imageBatchStyleA1111:
		// batch_size картинок за одну итерацию — ровно то, что нужно задаче.
		if err := set("batch_size", count); err != nil {
			return nil, err
		}
		if err := set("n_iter", 1); err != nil {
			return nil, err
		}
	default:
		if err := set("n", count); err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

// mergeImageBatchResponsesR99 — склеить ответы задач в один ответ поверхности.
//
// Порядок картинок = порядок задач плана: клиент, попросивший 4 картинки, должен
// получить их в предсказуемом порядке, а не «как успели».
func mergeImageBatchResponsesR99(style string, parts [][]byte) ([]byte, int, error) {
	if len(parts) == 0 {
		return nil, 0, fmt.Errorf(i18n.T("balancer.image_batch.empty_responses"))
	}
	switch style {
	case imageBatchStyleA1111:
		merged := map[string]json.RawMessage{}
		images := make([]json.RawMessage, 0, 4)
		for i, part := range parts {
			var payload struct {
				Images     []json.RawMessage `json:"images"`
				Parameters json.RawMessage   `json:"parameters"`
				Info       json.RawMessage   `json:"info"`
			}
			if err := json.Unmarshal(part, &payload); err != nil {
				return nil, 0, fmt.Errorf(i18n.T("balancer.image_batch.part_not_json"), i+1, err)
			}
			images = append(images, payload.Images...)
			if i == 0 {
				// parameters/info описывают ЗАПРОС, а не картинку: берём от первой
				// задачи, иначе в ответе окажется смесь настроек разных узлов.
				if len(payload.Parameters) > 0 {
					merged["parameters"] = payload.Parameters
				}
				if len(payload.Info) > 0 {
					merged["info"] = payload.Info
				}
			}
		}
		rawImages, err := json.Marshal(images)
		if err != nil {
			return nil, 0, err
		}
		merged["images"] = rawImages
		out, err := json.Marshal(merged)
		return out, len(images), err

	default:
		merged := map[string]json.RawMessage{}
		data := make([]json.RawMessage, 0, 4)
		for i, part := range parts {
			var payload struct {
				Created int64             `json:"created"`
				Data    []json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(part, &payload); err != nil {
				return nil, 0, fmt.Errorf(i18n.T("balancer.image_batch.part_not_json"), i+1, err)
			}
			data = append(data, payload.Data...)
			if i == 0 && payload.Created > 0 {
				raw, err := json.Marshal(payload.Created)
				if err != nil {
					return nil, 0, err
				}
				merged["created"] = raw
			}
		}
		rawData, err := json.Marshal(data)
		if err != nil {
			return nil, 0, err
		}
		merged["data"] = rawData
		out, err := json.Marshal(merged)
		return out, len(data), err
	}
}

// imageBatchPartR99 — исход одной задачи пачки.
type imageBatchPartR99 struct {
	body      []byte
	err       error
	code      string
	message   string
	backendID string
	status    int
	count     int
}

// routeImageBatchR99 — раскладка пачки по нескольким image-бэкендам.
//
// Возвращает true, если запрос обслужен (или отвергнут) здесь. false означает
// «пачка неприменима» — вызывающий идёт обычным одиночным путём, ничего не
// заметив (тело запроса восстановлено).
func (ir *ImageRouter) routeImageBatchR99(w http.ResponseWriter, r *http.Request, store *imageRequestStore, meta types.ImageRequestBrief) bool {
	if ir == nil || ir.proxy == nil || r == nil {
		return false
	}
	body, ok := readImageBodyR99(r)
	if !ok {
		return false
	}
	count, style, ok := imageBatchSpecR99(body, r.URL.Path)
	if !ok || count <= 1 {
		return false
	}

	candidates := ir.imageBatchCandidatesR99()
	if len(candidates) < 2 {
		// Один image-бэкенд — раскладывать не по чему: обычный путь.
		return false
	}

	// Гейт и лок — НА КАЖДЫЙ узел пачки. Узел, чей гейт отказал (VRAM/лок), в
	// раскладке не участвует: иначе пачка обошла бы защиту, ради которой гейт и
	// сделан.
	gates := make([]imageGateDecision, 0, len(candidates))
	used := make([]string, 0, len(candidates))
	defer func() {
		for _, g := range gates {
			g.Release()
		}
	}()
	for _, id := range candidates {
		if len(used) >= count {
			break
		}
		gate := ir.proxy.imageResources().beforeGeneration(r.Context(), id)
		if !gate.Allowed {
			logger.Get().Infow(i18n.T("balancer.image_batch.node_skipped_info"),
				"backend", id, "code", gate.Code, "count", count)
			continue
		}
		gates = append(gates, gate)
		used = append(used, id)
	}
	if len(used) < 2 {
		// Свободен только один узел — пачка не даст ускорения и лишь усложнит
		// отказ. Возвращаемся к одиночному пути (гейты снимет defer).
		return false
	}

	plan := imageBatchPlanR99(count, used)
	if len(plan) < 2 {
		return false
	}

	parts := ir.fanOutImageBatchR99(r, body, style, plan, store, meta)
	if failed, ok := firstFailedPartR99(parts); !ok {
		ir.writeBatchUpstreamErrorR99(w, failed)
		return true
	}

	merged, images, err := mergeImageBatchResponsesR99(style, bodiesOfR99(parts))
	if err != nil {
		logger.Get().Errorw(i18n.T("balancer.image_batch.merge_failed_error"), "error", err)
		writeUpstreamError(w, parts[0].backendID, meta.Model, err)
		return true
	}

	w.Header().Set("Content-Type", "application/json")
	// Следы раскладки в ответе: на живом стенде по ним видно, что пачка реально
	// разошлась, а не «повезло».
	w.Header().Set("X-LB-Image-Batch", fmt.Sprintf("%d backends / %d images", len(plan), count))
	w.Header().Set("X-LB-Image-Batch-Parts", describePartsR99(parts))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(merged)

	logger.Get().Infow("image batch served",
		"path", r.URL.Path, "requested", count, "returned", images,
		"backends", len(plan), "parts", describePartsR99(parts))
	return true
}

// fanOutImageBatchR99 — параллельный обход задач пачки.
//
// Параллельно, а не по очереди: AC S8 требует «wall-time ≈ половина одиночного»
// на двух узлах, а последовательный обход дал бы ровно то же время, что и один
// узел, — то есть ноль пользы при вдвое большей сложности.
func (ir *ImageRouter) fanOutImageBatchR99(
	r *http.Request,
	body []byte,
	style string,
	plan []imageBatchChunkR99,
	store *imageRequestStore,
	meta types.ImageRequestBrief,
) []imageBatchPartR99 {
	parts := make([]imageBatchPartR99, len(plan))
	var wg sync.WaitGroup

	for i := range plan {
		chunk := plan[i]
		wg.Add(1)
		go func(idx int, chunk imageBatchChunkR99) {
			defer wg.Done()
			part := imageBatchPartR99{backendID: chunk.BackendID, count: chunk.Count}
			parts[idx] = part

			chunkBody, err := imageBatchBodyR99(body, style, chunk.Count)
			if err != nil {
				parts[idx].err = err
				return
			}

			// Каждая задача — отдельная запись в статистике image-запросов:
			// оператор должен видеть, что пачка разошлась, и по какому узлу.
			chunkMeta := meta
			chunkMeta.Batch = chunk.Count
			handle := store.begin(chunk.BackendID, chunkMeta)

			reqCopy := r.Clone(r.Context())
			reqCopy.Body = io.NopCloser(bytes.NewReader(chunkBody))
			reqCopy.ContentLength = int64(len(chunkBody))
			reqCopy.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(chunkBody)), nil
			}

			resp, err := ir.proxy.proxyRequestToBackend(reqCopy, chunk.BackendID, imageRequestTimeout())
			if err != nil {
				errType := UpstreamErrorType(err)
				if handle != nil {
					handle.finish(types.ImageRequestStatusFailed, UpstreamErrorStatus(errType),
						"image_"+errType, err.Error(), 0)
				}
				parts[idx].err = err
				return
			}

			code, msg := "", ""
			if resp.StatusCode >= 400 {
				code, msg = peekImageErrorBody(resp)
			}
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, imageBatchPartLimitR99))
			_ = resp.Body.Close()
			if readErr != nil {
				if handle != nil {
					handle.finish(types.ImageRequestStatusFailed, resp.StatusCode,
						"image_batch_read_error", readErr.Error(), 0)
				}
				parts[idx].err = readErr
				return
			}

			status := types.ImageRequestStatusOK
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				status = types.ImageRequestStatusFailed
				if code == "" {
					code = "image_backend_http_" + itoaSafeR99(resp.StatusCode)
				}
			}
			if handle != nil {
				handle.finish(status, resp.StatusCode, code, msg, imageResponseImages(chunkMeta, resp.StatusCode))
			}
			parts[idx].body = raw
			parts[idx].status = resp.StatusCode
			parts[idx].code = code
			parts[idx].message = msg
		}(i, chunk)
	}
	wg.Wait()
	return parts
}

// firstFailedPartR99 — первая упавшая задача (ok=false означает «всё в порядке»).
func firstFailedPartR99(parts []imageBatchPartR99) (imageBatchPartR99, bool) {
	for _, part := range parts {
		if part.err != nil {
			return part, false
		}
		if part.status < 200 || part.status >= 300 {
			return part, false
		}
	}
	return imageBatchPartR99{}, true
}

// writeBatchUpstreamErrorR99 — отказ пачки с указанием узла и задачи.
func (ir *ImageRouter) writeBatchUpstreamErrorR99(w http.ResponseWriter, part imageBatchPartR99) {
	if part.err != nil {
		logger.Get().Errorw("image batch part failed",
			"backend", part.backendID, "count", part.count, "error", part.err)
		writeUpstreamError(w, part.backendID, "", part.err)
		return
	}
	logger.Get().Warnw("image batch part returned error status",
		"backend", part.backendID, "count", part.count,
		"status", part.status, "code", part.code, "message", part.message)
	// Ответ узла отдаём как есть: это ошибка его движка, и клиенту полезнее
	// исходный текст, чем наша формулировка.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-LB-Image-Batch", "partial-failure")
	w.Header().Set("X-LB-Image-Batch-Failed-Backend", part.backendID)
	w.WriteHeader(part.status)
	_, _ = w.Write(part.body)
}

// imageBatchCandidatesR99 — здоровые image-бэкенды в детерминированном порядке.
//
// Скоринг здесь не нужен: задачи идут ПАРАЛЛЕЛЬНО, и важен не «лучший узел», а
// «сколько узлов готовы» — пригодность конкретного узла проверяет гейт.
func (ir *ImageRouter) imageBatchCandidatesR99() []string {
	if ir == nil || ir.proxy == nil {
		return nil
	}
	states := ir.proxy.filterBackendsByType(types.BackendTypeImage)
	ids := make([]string, 0, len(states))
	for _, state := range states {
		if state == nil || state.Backend == nil {
			continue
		}
		ids = append(ids, state.Backend.ID)
	}
	sort.Strings(ids)
	return ids
}

// readImageBodyR99 — тело запроса с восстановлением (вызывающий может продолжить
// обычным путём, если пачка не применилась).
func readImageBodyR99(r *http.Request) ([]byte, bool) {
	if r == nil || r.Body == nil {
		return nil, true
	}
	if r.ContentLength > imageBatchBodyLimitR99 {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, imageBatchBodyLimitR99+1))
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) > imageBatchBodyLimitR99 {
		return nil, false
	}
	return body, true
}

func bodiesOfR99(parts []imageBatchPartR99) [][]byte {
	out := make([][]byte, 0, len(parts))
	for _, part := range parts {
		out = append(out, part.body)
	}
	return out
}

// describePartsR99 — «img-1=2,img-2=2» для заголовка и лога.
func describePartsR99(parts []imageBatchPartR99) string {
	list := make([]string, 0, len(parts))
	for _, part := range parts {
		list = append(list, fmt.Sprintf("%s=%d", part.backendID, part.count))
	}
	return strings.Join(list, ",")
}

func itoaSafeR99(v int) string {
	return fmt.Sprintf("%d", v)
}
