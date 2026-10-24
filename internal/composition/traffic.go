package composition

import "strings"

// Нормировки и константы валидатора.

const (
	// defaultVRAMSafetyFactor — запас к весам на KV и накладные (как в
	// placement-fit, internal/balancer/placement_fit_r75.go).
	defaultVRAMSafetyFactor = 1.25
	// defaultPrefillTokens — длина префилла для оценки трафика по умолчанию
	// (уровень, на котором в §3 плана посчитаны 37–75 мс).
	defaultPrefillTokens = 512
	// rowSplitWarnPercent — с какого превышения времени токена row split
	// считается предупреждением (§7: «> 10 % времени токена»).
	rowSplitWarnPercent = 10.0

	// StrategyLayer — pipeline split: через сеть идёт активация на границе.
	StrategyLayer = "layer"
	// StrategyRow — row/tensor split: каждый слой обменивается партиалами.
	StrategyRow = "row"
)

// NormalizeStrategy приводит стратегию раскладки к layer/row ("" → layer).
func NormalizeStrategy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case StrategyRow, "tensor", "row_split":
		return StrategyRow
	case StrategyLayer, "pipeline", "layer_split":
		return StrategyLayer
	case "":
		return StrategyLayer
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

// NormalizeBackendType приводит тип бэкенда к каноническому виду.
//
// Пустой тип — это ollama: так же считает normalizeBackendType в балансере
// (историческая совместимость с конфигами без поля type).
func NormalizeBackendType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "ollama":
		return "ollama"
	case "llama_cpp", "llamacpp", "cppworker", "llama.cpp":
		return "llama_cpp"
	case "image_cpp", "image", "sdworker":
		return "image_cpp"
	default:
		return strings.ToLower(strings.TrimSpace(t))
	}
}

// TrafficInput — вход оценки сетевого бюджета раскладки (§3 плана).
//
// Порядок полей — по требованию fieldalignment.
type TrafficInput struct {
	Strategy             string
	PrefillTokens        int
	HiddenSize           int
	Layers               int
	BytesPerElem         int
	BandwidthBytesPerSec float64
	TokensPerSecond      float64
}

// TrafficEstimate — результат оценки.
//
// TokenOverheadPercent < 0 означает «неизвестно»: скорость декода не задана, и
// доля от времени токена не считается (абсолютная задержка при этом известна).
//
// Порядок полей — по требованию fieldalignment.
type TrafficEstimate struct {
	Reason               string
	BytesPerToken        int64
	BytesPerPrefill      int64
	TokenOverheadMs      float64
	PrefillMs            float64
	TokenOverheadPercent float64
	Known                bool
}

// EstimateTraffic считает трафик и его вклад в задержку по формулам §3:
//
//	layer split: H × bytes на токен,       B × H × bytes на префилл B
//	row split:   H × bytes × L на токен,   B × H × L × bytes на префилл B
//
// Границу раскладки батч пересекает один раз за проход (layer), поэтому числа
// отличаются в L раз — это и есть причина, по которой на 1 Гбит/с рабочим
// вариантом является layer, а row включается осознанно.
func EstimateTraffic(in TrafficInput) TrafficEstimate {
	estimate := TrafficEstimate{TokenOverheadPercent: -1}
	if in.HiddenSize <= 0 {
		estimate.Reason = "hidden size неизвестен"
		return estimate
	}
	if in.BandwidthBytesPerSec <= 0 {
		estimate.Reason = "пропускная способность канала неизвестна"
		return estimate
	}
	bytesPerElem := in.BytesPerElem
	if bytesPerElem <= 0 {
		bytesPerElem = 2 // f16
	}

	perToken := int64(in.HiddenSize) * int64(bytesPerElem)
	switch NormalizeStrategy(in.Strategy) {
	case StrategyRow:
		if in.Layers <= 0 {
			estimate.Reason = "число слоёв неизвестно: row split без него не оценить"
			return estimate
		}
		perToken *= int64(in.Layers)
	}

	prefillTokens := in.PrefillTokens
	if prefillTokens <= 0 {
		prefillTokens = defaultPrefillTokens
	}

	estimate.Known = true
	estimate.BytesPerToken = perToken
	estimate.BytesPerPrefill = perToken * int64(prefillTokens)
	estimate.TokenOverheadMs = float64(perToken) / in.BandwidthBytesPerSec * 1000
	estimate.PrefillMs = float64(estimate.BytesPerPrefill) / in.BandwidthBytesPerSec * 1000
	if in.TokensPerSecond > 0 {
		tokenTimeMs := 1000 / in.TokensPerSecond
		estimate.TokenOverheadPercent = estimate.TokenOverheadMs / tokenTimeMs * 100
	}
	return estimate
}
