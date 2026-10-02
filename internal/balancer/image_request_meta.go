// image_request_meta.go — R-Image Phase 8 (2026-10-03): метаданные image-запроса
// для ленты метрик и разбор ошибки движка.
//
// ГРАНИЦЫ (почему так, а не «читать тело целиком»):
//
//   - Тело читается ТОЛЬКО у небольших JSON-запросов генерации и только когда
//     Content-Length известен и не превышает imageRequestMetaBodyLimit. У
//     /v1/images/edits это multipart с картинкой (мегабайты base64): буферизовать
//     его в балансере ради строки в ленте нельзя, поэтому такие запросы дают
//     минимум (путь + исход).
//   - Тело восстанавливается (io.NopCloser(bytes.NewReader)): проксирование
//     читает его ещё раз и обязано получить исходные байты.
//   - Ответ НЕ буферизуется целиком: у ошибок подглядываем не больше
//     imageResponsePeekLimit байт, чтобы достать code/message движка, и
//     возвращаем поток на место.
package balancer

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const (
	// imageRequestMetaBodyLimit — максимальный размер тела, которое разбираем
	// ради метаданных (модель/размер/шаги/промпт).
	imageRequestMetaBodyLimit = 64 << 10
	// imageResponsePeekLimit — сколько байт тела ошибки читаем для code/message.
	imageResponsePeekLimit = 4 << 10
)

// imageSizeRe — OpenAI-размер вида "512x512" (WxH).
var imageSizeRe = regexp.MustCompile(`^\s*(\d{2,5})\s*[x×]\s*(\d{2,5})\s*$`)

// imageRequestBriefFor — снимок «что именно запросили» для ленты метрик.
//
// Никогда не ломает запрос: при любой неопределённости возвращает только
// поверхность и путь (лучше бедная метрика, чем испорченное тело).
func imageRequestBriefFor(r *http.Request) types.ImageRequestBrief {
	brief := types.ImageRequestBrief{At: time.Now()}
	if r == nil {
		return brief
	}
	brief.Surface = requestSurfaceOf(r).String()
	brief.Path = r.URL.Path
	if r.Body == nil {
		return brief
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return brief
	}
	// ContentLength < 0 = chunked/неизвестно: читать нельзя (прочитаем лишь
	// префикс, а восстановить сможем только его — запрос уедет обрезанным).
	if r.ContentLength <= 0 || r.ContentLength > imageRequestMetaBodyLimit {
		return brief
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, imageRequestMetaBodyLimit+1))
	// Восстанавливаем тело в ЛЮБОМ случае — даже если разбор не удался.
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) > imageRequestMetaBodyLimit {
		return brief
	}

	var payload struct {
		Model          string          `json:"model"`
		Prompt         json.RawMessage `json:"prompt"`
		NegativePrompt string          `json:"negative_prompt"`
		Width          int             `json:"width"`
		Height         int             `json:"height"`
		Size           string          `json:"size"`
		Steps          int             `json:"steps"`
		BatchCount     int             `json:"batch_count"`
		BatchSize      int             `json:"batch_size"`
		N              int             `json:"n"`
		NIter          int             `json:"n_iter"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return brief
	}

	brief.Model = strings.TrimSpace(payload.Model)
	brief.Width = payload.Width
	brief.Height = payload.Height
	if (brief.Width == 0 || brief.Height == 0) && payload.Size != "" {
		if m := imageSizeRe.FindStringSubmatch(payload.Size); m != nil {
			brief.Width = atoiSafe(m[1])
			brief.Height = atoiSafe(m[2])
		}
	}
	brief.Steps = payload.Steps
	// Число картинок в запросе. У OpenAI это n, у A1111 — ПРОИЗВЕДЕНИЕ
	// batch_size × n_iter (это разные вещи: batch_size картинок на итерацию,
	// n_iter итераций), поэтому просто взять первое положительное поле нельзя.
	batch := payload.BatchSize
	if payload.NIter > 0 {
		if batch > 0 {
			batch *= payload.NIter
		} else {
			batch = payload.NIter
		}
	}
	if batch <= 0 {
		batch = firstPositive(payload.BatchCount, payload.N, 1)
	}
	brief.Batch = batch
	brief.Prompt = truncateRunes(imagePromptFromRaw(payload.Prompt), imagePromptLimit)
	return brief
}

// imagePromptFromRaw — промпт из тела: строка (OpenAI) или первый элемент
// массива (A1111 допускает массив промптов).
func imagePromptFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr[0]
	}
	return ""
}

// peekImageErrorBody — code/message из тела ошибки движка (OpenAI-конверт или
// плоский), не более imageResponsePeekLimit байт. Поток ответа восстанавливается.
func peekImageErrorBody(resp *http.Response) (code, msg string) {
	if resp == nil || resp.Body == nil {
		return "", ""
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		return "", ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, imageResponsePeekLimit+1))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) > imageResponsePeekLimit {
		return "", ""
	}

	var envelope struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
		Msg   string          `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", ""
	}
	if len(envelope.Error) > 0 {
		// OpenAI-форма: {"error":{"message","type","code"}}.
		var nested struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		}
		if err := json.Unmarshal(envelope.Error, &nested); err == nil && (nested.Message != "" || nested.Code != "") {
			return nested.Code, truncateRunes(nested.Message, imagePromptLimit)
		}
		// sd.cpp-форма: {"error":"строка"}.
		var plain string
		if err := json.Unmarshal(envelope.Error, &plain); err == nil {
			return "", truncateRunes(plain, imagePromptLimit)
		}
	}
	return envelope.Code, truncateRunes(envelope.Msg, imagePromptLimit)
}

// imageResponseImages — сколько картинок ожидается в ответе.
//
// Считаем по ПАРАМЕТРАМ запроса, а не по телу ответа: тело синхронной
// генерации — это base64 (сотни килобайт), и буферизовать его ради счётчика
// нельзя. brief.Batch уже сведён из параметров конкретной поверхности: у OpenAI
// это n, у A1111 — batch_size × n_iter (см. imageRequestBriefFor).
func imageResponseImages(brief types.ImageRequestBrief, status int) int {
	if status < 200 || status >= 300 {
		return 0
	}
	if brief.Batch > 0 {
		return brief.Batch
	}
	return 1
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			return 0
		}
	}
	return n
}

// truncateRunes — обрезка по РУНАМ (кириллица в промпте: обрезка по байтам
// разрезала бы символ и в JSON уехал бы мусор).
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}
