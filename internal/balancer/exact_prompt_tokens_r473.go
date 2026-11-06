// exact_prompt_tokens_r473.go — R473 (2026-11-06): preflight спрашивает ТОЧНОЕ
// число токенов промпта у токенизатора модели, а не только у эвристики.
//
// ЗАЧЕМ. Вся оконная арифметика балансера стоит на `pkg/tokencount` — оценке по
// символам. Живые измерения на этом стенде: эвристика давала 49731 при реальных
// 9816, 36601 при реальных ~20.7k (Cline считает по нашему же usage) — завышение
// 1.8–5×. Из завышенного числа рождались: отказы 413 (до R466), отменённый рост
// окна (до R472) и перезагрузки модели на окно БОЛЬШЕ нужного (после R472:
// грузили 40960 там, где хватало ~33k).
//
// Токенизатор есть только в воркере, а с R467 он отдаёт его наружу:
//
//	POST /api/tokenize {"model": "...", "text": "..."} →
//	{tokens, source: "tokenizer"|"estimate", model_loaded, notice}
//
// Здесь это используется так: мета запроса несёт ТОТ ЖЕ текст, который пошёл в
// эвристику (плюс сериализованные tools[], потому что они реально занимают место
// в промпте), мы отправляем его на бэкенд, где модель загружена, и при
// source=="tokenizer" заменяем оценку точным числом + небольшим запасом на
// системную преамбулу шаблона.
//
// ЧЕГО НЕ ДЕЛАЕМ:
//   - не трогаем эвристику и калибровку: они остаются фолбэком (модель не
//     загружена, воркер недоступен, таймаут, старый воркер без /api/tokenize);
//   - не обучаем калибровку по точному числу — иначе две оценки начнут
//     «подгонять» друг друга;
//   - не ходим в сеть на мелких промптах: там завышение не влияет на решения,
//     а лишний запрос на горячем пути не нужен.
package balancer

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// exactTokensSwitchEnvR473 — рубильник. По умолчанию ВКЛЮЧЁН: именно он убирает
// завышение, из-за которого модель грузится на лишнее окно.
const exactTokensSwitchEnvR473 = "LB_PREFLIGHT_EXACT_TOKENS"

// exactTokensMinSampleR473 — ниже этого размера образца (в байтах) точность не
// влияет на решения: 4 КБ ≈ 1–2k токенов, а пороги окон начинаются с 2–16k.
const exactTokensMinSampleR473 = 4096

// exactTokensTimeoutR473 — жёсткий предел на вызов токенизатора. Это горячий
// путь: лучше остаться с оценкой, чем держать запрос.
const exactTokensTimeoutR473 = 1500 * time.Millisecond

// exactTokensOverheadTokensR473 — запас на то, чего нет в образце: системная
// преамбула, служебные токены шаблона, разделители ролей.
const exactTokensOverheadTokensR473 = 32

// exactTokensEnabledR473 — включён ли точный подсчёт (значение по умолчанию — да).
func exactTokensEnabledR473() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(exactTokensSwitchEnvR473)))
	switch v {
	case "", "1", "true", "yes", "on", "auto":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		logger.Get().Warnw("LB_PREFLIGHT_EXACT_TOKENS: неизвестное значение, оставляю включённым",
			"value", v)
		return true
	}
}

// tokenizeResponseR473 — ответ воркера (R467), только нужные поля.
// Порядок полей — под fieldalignment (govet): указатели впереди, числа и bool
// после. На JSON не влияет (имена заданы тегами).
type tokenizeResponseR473 struct {
	Source      string `json:"source"`
	Tokens      int    `json:"tokens"`
	ModelLoaded bool   `json:"model_loaded"`
}

// exactPromptTokensR473 — точное число токенов образца промпта.
//
// Возвращает (tokens, true) только когда его посчитал РЕАЛЬНЫЙ токенизатор
// загруженной модели. Во всех остальных случаях (false) вызывающий остаётся при
// эвристике — это не ошибка, а честный фолбэк.
func (p *Proxy) exactPromptTokensR473(meta *RequestMeta) (int, bool) {
	if p == nil || meta == nil || meta.ModelName == "" {
		return 0, false
	}
	sample := meta.ExactSampleText
	if len(sample) < exactTokensMinSampleR473 {
		return 0, false
	}
	if !exactTokensEnabledR473() {
		return 0, false
	}
	backendID := p.backendWithLoadedModelR301(meta.ModelName)
	if backendID == "" {
		// Модель нигде не загружена — токенизатора нет, оценка остаётся.
		return 0, false
	}
	addr := p.backendHTTPAddrByID(backendID)
	if addr == "" {
		return 0, false
	}
	n, ok := p.callTokenizeR473(addr, meta.ModelName, sample)
	if !ok {
		return 0, false
	}
	return n + exactTokensOverheadTokensR473, true
}

// callTokenizeR473 — HTTP-вызов /api/tokenize. Отдельная функция, чтобы её можно
// было проверить тестом без Proxy и без живого воркера.
func (p *Proxy) callTokenizeR473(baseURL, model, text string) (int, bool) {
	body, err := json.Marshal(map[string]string{"model": model, "text": text})
	if err != nil {
		return 0, false
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/tokenize",
		bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	// Тот же токен, что балансер использует для остальных ручек воркера:
	// /api/tokenize закрыт authMiddleware.
	for _, key := range []string{"CPPWORKER_API_TOKEN", "LB_API_TOKEN"} {
		if tok := strings.TrimSpace(os.Getenv(key)); tok != "" {
			req.Header.Set("X-API-Token", tok)
			break
		}
	}
	client := &http.Client{Timeout: exactTokensTimeoutR473}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var out tokenizeResponseR473
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, false
	}
	if out.Source != "tokenizer" || out.Tokens <= 0 {
		return 0, false
	}
	return out.Tokens, true
}

// exactSampleWithToolsR473 — образец для точного подсчёта: текст сообщений плюс
// сериализованные tools[].
//
// ПОЧЕМУ С TOOLS. Схемы инструментов реально занимают место в промпте: у Cline
// их 6, у DSH — 35 (это ~12k токенов на ровном месте). Эвристика оценивала
// только текст сообщений, поэтому на агентских запросах систематически занижала
// долю tools, а калибровка потом «лечила» это общим коэффициентом. Точный
// подсчёт должен видеть то же, что увидит шаблон, иначе смысла в нём нет.
func exactSampleWithToolsR473(text string, tools any) string {
	if tools == nil {
		return text
	}
	encoded, err := json.Marshal(tools)
	if err != nil || len(encoded) == 0 || string(encoded) == "null" || string(encoded) == "[]" {
		return text
	}
	if text == "" {
		return string(encoded)
	}
	return text + "\n" + string(encoded)
}

// applyExactPromptTokensR473 — заменить калиброванную оценку точным числом, если
// оно есть. Вызывается из applyPromptCalibration — единственной точки, через
// которую решения (preflight, growth guard, reload) читают метаданные запроса.
//
// Калибровку при этом НЕ обучаем (promptCalib.note уже сделан по эвристике):
// иначе точное число и коэффициент начнут подгонять друг друга.
func (p *Proxy) applyExactPromptTokensR473(meta *RequestMeta) {
	exact, ok := p.exactPromptTokensR473(meta)
	if !ok {
		return
	}
	estimated := meta.EstimatedPromptTokens
	meta.EstimatedPromptTokens = exact
	meta.PromptTokensSource = "tokenizer"
	logger.Get().Infow("prompt tokens: точный подсчёт токенизатором (R473)",
		"model", meta.ModelName,
		"estimated_tokens", estimated,
		"exact_tokens", exact,
		"sample_bytes", len(meta.ExactSampleText),
		"note", "оценка заменена фактом: решения об окне считаются по реальному числу")
}
