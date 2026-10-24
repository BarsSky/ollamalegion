package composition

import (
	"fmt"
	"strings"
)

// Тексты замечаний на двух языках репозитория (ru/en).
//
// ПОЧЕМУ НЕ ШАБЛОНЫ С ПОДСТАНОВКОЙ. Единый текст ошибок требуется §7 плана, но
// формулировки сильно различаются по составу чисел (где-то есть максимум по узлу,
// где-то нет), а порядок и число аргументов у ru/en совпадают не всегда. Поэтому
// текст собирается функцией на каждый CheckID: читается как обычная строка, легко
// проверяется тестом, и не требует движка шаблонов.
//
// UI (S5/S6) опирается на CheckID + Params и рисует СВОИ подписи из
// webui/js/i18n/*.js — этот текст нужен логам, CLI и API-ответу «как есть».

// LangRU / LangEN — поддерживаемые языки текстов.
const (
	LangRU = "ru"
	LangEN = "en"
)

// NormalizeLang приводит язык к поддерживаемому (неизвестный → ru).
func NormalizeLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case LangEN, "en-us", "en_us":
		return LangEN
	default:
		return LangRU
	}
}

// gpuGroupParam — группы однородности для текста проверки homogeneous.
type gpuGroupParam struct {
	// Identity — «личность» GPU; пустая = неизвестна.
	Identity string
	// Backends — узлы с этой «личностью».
	Backends []string
}

// renderText собирает человекочитаемый текст замечания.
func renderText(check CheckID, lang string, p map[string]any) string {
	switch check {
	case CheckNodeUnreachable:
		backend, role := str(p["backend"]), str(p["role"])
		if str(p["reason"]) == "unregistered" {
			if lang == LangEN {
				return fmt.Sprintf("node %s (%s) is not registered in the balancer", backend, role)
			}
			return fmt.Sprintf("узел %s (%s) не зарегистрирован в балансере", backend, role)
		}
		if lang == LangEN {
			return fmt.Sprintf("node %s (%s) is not healthy: status %s", backend, role, str(p["status"]))
		}
		return fmt.Sprintf("узел %s (%s) не healthy: статус %s", backend, role, str(p["status"]))

	case CheckRPCUnreachable:
		backend, endpoint := str(p["backend"]), str(p["endpoint"])
		if errText := str(p["error"]); errText != "" {
			if lang == LangEN {
				return fmt.Sprintf("node %s does not answer on %s (%s)", backend, endpoint, errText)
			}
			return fmt.Sprintf("узел %s не отвечает на %s (%s)", backend, endpoint, errText)
		}
		if lang == LangEN {
			return fmt.Sprintf("node %s does not answer on %s", backend, endpoint)
		}
		return fmt.Sprintf("узел %s не отвечает на %s", backend, endpoint)

	case CheckVersionMismatch:
		if lang == LangEN {
			return "llama.cpp builds differ between RPC peers: " + str(p["detail"]) +
				" — the RPC protocol is version-sensitive, load will time out instead of failing clearly"
		}
		return "версии llama.cpp у сторон RPC не совпадают: " + str(p["detail"]) +
			" — протокол RPC версионно-чувствителен, загрузка даст таймаут вместо понятной ошибки"

	case CheckVersionUnknown:
		if lang == LangEN {
			return "llama.cpp version is not reported by: " + str(p["detail"]) +
				" — RPC protocol compatibility is not confirmed"
		}
		return "версия llama.cpp не сообщается узлами: " + str(p["detail"]) +
			" — совместимость протокола RPC не подтверждена"

	case CheckVRAMSumInsufficient:
		need := humanBytes(lang, int64(num(p["needBytes"])))
		weights := humanBytes(lang, int64(num(p["weightsMB"]))*1024*1024)
		available := humanBytes(lang, int64(num(p["availableMB"]))*1024*1024)
		maxNode := humanBytes(lang, int64(num(p["maxNodeMB"]))*1024*1024)
		if lang == LangEN {
			return fmt.Sprintf("need ~%s (weights %s + %.0f%% headroom), available %s across %d node(s) "+
				"(max on a single node %s)", need, weights, (numF(p["safety"])-1)*100, available,
				num(p["nodeCount"]), maxNode)
		}
		return fmt.Sprintf("нужно ≈%s (веса %s + запас %.0f%%), доступно %s по %d узл(ам) "+
			"(максимум на одном узле %s)", need, weights, (numF(p["safety"])-1)*100, available,
			num(p["nodeCount"]), maxNode)

	case CheckKVRAMInsufficient:
		ctx := num(p["nCtx"])
		kv := humanBytes(lang, int64(num(p["kvBytes"])))
		available := humanBytes(lang, int64(num(p["availableBytes"])))
		ram := humanBytes(lang, int64(num(p["ramFreeMB"]))*1024*1024)
		vramLeft := humanBytes(lang, int64(num(p["vramLeftMB"]))*1024*1024)
		if lang == LangEN {
			return fmt.Sprintf("KV at ctx=%d requires %s, %s free (RAM %s + VRAM left %s)",
				ctx, kv, available, ram, vramLeft)
		}
		return fmt.Sprintf("KV при ctx=%d требует %s, свободно %s (RAM %s + остаток VRAM %s)",
			ctx, kv, available, ram, vramLeft)

	case CheckKVUnknown:
		if lang == LangEN {
			return fmt.Sprintf("n_ctx=%d is not checked: KV parameters (n_kv_heads/head_dim) are unknown",
				num(p["nCtx"]))
		}
		return fmt.Sprintf("n_ctx=%d не проверен: параметры KV (n_kv_heads/head_dim) неизвестны",
			num(p["nCtx"]))

	case CheckTrafficRowHigh:
		bandwidth := numF(p["gbps"])
		if lang == LangEN {
			return fmt.Sprintf("row split: ~+%.1f%% to token time, ~%.1f s per %d-token prefill at %.2f Gbit/s",
				numF(p["overheadPct"]), numF(p["prefillMs"])/1000, num(p["prefillTokens"]), bandwidth)
		}
		return fmt.Sprintf("row split: ≈+%.1f %% к времени токена, ≈%.1f с на префилл %d при %.2f Гбит/с",
			numF(p["overheadPct"]), numF(p["prefillMs"])/1000, num(p["prefillTokens"]), bandwidth)

	case CheckFitsSingle:
		backend := str(p["backend"])
		available := humanBytes(lang, int64(num(p["availableMB"]))*1024*1024)
		need := humanBytes(lang, int64(num(p["needMB"]))*1024*1024)
		if lang == LangEN {
			return fmt.Sprintf("the model already fits one node (%s: %s free vs %s needed) — "+
				"single is enough; replicated would give more throughput", backend, available, need)
		}
		return fmt.Sprintf("модель влезает в один узел (%s: свободно %s при нужных %s) — "+
			"достаточно single; replicated даст больше пропускной способности", backend, available, need)

	case CheckBackendType:
		backend := str(p["backend"])
		nodeType, modelType := str(p["nodeType"]), str(p["modelType"])
		if lang == LangEN {
			return fmt.Sprintf("node %s of type %s does not serve models of type %s", backend, nodeType, modelType)
		}
		return fmt.Sprintf("узел %s типа %s не обслуживает модели типа %s", backend, nodeType, modelType)

	case CheckHomogeneous:
		groups, _ := p["groups"].([]gpuGroupParam)
		parts := make([]string, 0, len(groups))
		for _, g := range groups {
			identity := g.Identity
			if identity == "" {
				if lang == LangEN {
					identity = "unknown"
				} else {
					identity = "неизвестно"
				}
			}
			parts = append(parts, identity+"="+strings.Join(g.Backends, ","))
		}
		detail := strings.Join(parts, "; ")
		if detail == "" {
			detail = str(p["detail"])
		}
		if lang == LangEN {
			return "group GPUs are not homogeneous (" + detail + "), but the rule requires identical GPUs"
		}
		return "узлы группы различаются по GPU (" + detail + "), а правило требует одинаковые GPU"
	}
	return string(check)
}

// HumanBytes — внешний хелпер форматирования размера (логи/тесты).
func HumanBytes(lang string, bytes int64) string {
	return humanBytes(NormalizeLang(lang), bytes)
}

// Минимальный размер, который вообще форматируется в байтах.

func humanBytes(lang string, bytes int64) string {
	const (
		mb = int64(1024 * 1024)
		gb = 1024 * mb
	)
	switch {
	case bytes <= 0:
		if lang == LangEN {
			return "0 B"
		}
		return "0 Б"
	case bytes >= gb:
		unit := "ГБ"
		if lang == LangEN {
			unit = "GB"
		}
		return fmt.Sprintf("%.1f %s", float64(bytes)/float64(gb), unit)
	case bytes >= mb:
		unit := "МБ"
		if lang == LangEN {
			unit = "MB"
		}
		return fmt.Sprintf("%.0f %s", float64(bytes)/float64(mb), unit)
	default:
		unit := "Б"
		if lang == LangEN {
			unit = "B"
		}
		return fmt.Sprintf("%d %s", bytes, unit)
	}
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func num(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func numF(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}
