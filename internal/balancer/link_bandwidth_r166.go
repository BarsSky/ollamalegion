// link_bandwidth_r166.go — R166 (2026-10-17): какая пропускная способность канала
// идёт в оценку трафика раскладки, и откуда она взялась.
//
// ЗАЧЕМ ЭТОТ ФАЙЛ. До R166 число МБ/с в сухом прогоне бралось ТОЛЬКО из тела
// запроса, а его отсутствие подменялось константой 112 МБ/с (1 Гбит/с, §3 плана).
// На стенде с каналом 10 Гбит/с это давало оценку трафика в 10 раз пессимистичнее:
// композиция, которая прекрасно работает, могла быть отклонена «из-за сети».
//
// ЧТО ТЕПЕРЬ. Приоритет источников (сверху вниз), и в ответе ВСЕГДА видно, какой
// сработал:
//
//  1. request   — явное значение из запроса (оператор ввёл в WebUI или API);
//  2. auto      — АВТОЗАМЕР балансера до узла раскладки (/api/rpc-link-probe).
//                 Это ближайшая к реальности цифра: слои передаёт cppworker, то
//                 есть тот же путь «контейнер ↔ контейнер»;
//  3. env       — LB_RPC_LINK_MBPS оператора (замер хост↔хост скриптом);
//  4. default   — 112 МБ/с (исторический дефолт §3, 1 Гбит/с).
//
// ЕДИНИЦЫ ВНИМАТЕЛЬНО. В API запроса `bandwidthMBps` — МЕГАБАЙТЫ в секунду.
// Автозамер и LB_RPC_LINK_MBPS — МЕГАБИТЫ в секунду (так они и появились в
// проекте, и так их показывает панель: «889 Мбит/с (111 МБ/с)»). Перевод один:
// байт/с = Мбит/с × 1e6 / 8.
package balancer

import (
	"os"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Источники пропускной способности (для ответа API и панели).
const (
	LinkSourceRequest = "request"
	LinkSourceAuto    = "auto"
	LinkSourceEnv     = "env"
	LinkSourceDefault = "default"
)

// LinkBandwidthInfo — итог разрешения: что взяли и почему.
type LinkBandwidthInfo struct {
	// MBps — пропускная способность в МБ/с (как её ждёт оценка трафика).
	MBps float64 `json:"mbps"`
	// Mbps — то же в Мбит/с (как её показывает панель).
	Mbps int `json:"mbps_bits"`
	// Source — request | auto | env | default.
	Source string `json:"source"`
	// Detail — человекочитаемая причина: «замер балансера до узла X: 99 Мбит/с».
	Detail string `json:"detail,omitempty"`
	// BackendID — чей замер использован (только для source=auto).
	BackendID string `json:"backendId,omitempty"`
}

// BytesPerSec — значение для composition.Request.BandwidthBytesPerSec.
func (i LinkBandwidthInfo) BytesPerSec() float64 { return i.MBps * 1e6 }

// MeasuredLinkMBps — МИНИМАЛЬНЫЙ свежий авто-замер среди участников раскладки.
//
// Почему минимум: раскладка синхронна по шагам, и общий темп задаёт самый медленный
// канал до узла. Почему только участники (main/donor): у остальных узлов rpc-server
// не поднимается, и их канал к делу не относится.
func (p *Proxy) MeasuredLinkMBps() (int, string, bool) {
	if p == nil {
		return 0, "", false
	}
	best := 0
	bestID := ""
	for _, b := range p.GetAllBackends() {
		role := strings.TrimSpace(b.Role)
		if role != types.BackendRoleDonor && role != types.BackendRoleMain {
			continue
		}
		mbps, _, ok := p.RPCLinkMeasured(b.ID)
		if !ok || mbps <= 0 {
			continue
		}
		if best == 0 || mbps < best {
			best, bestID = mbps, b.ID
		}
	}
	if best == 0 {
		return 0, "", false
	}
	return best, bestID, true
}

// envLinkMBps — операторское значение LB_RPC_LINK_MBPS (в МЕГАБИТАХ, как в панели).
func envLinkMBps() (int, bool) {
	v := strings.TrimSpace(os.Getenv("LB_RPC_LINK_MBPS"))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		logger.Get().Warnw("LB_RPC_LINK_MBPS is not a positive number, ignoring",
			"value", v)
		return 0, false
	}
	return n, true
}

// LinkBandwidthInfo — разрешить пропускную способность для оценки трафика.
//
// requestedMBps — значение из тела запроса в МБ/с (0 = не задано).
func (p *Proxy) LinkBandwidthInfo(requestedMBps float64) LinkBandwidthInfo {
	if requestedMBps > 0 {
		return LinkBandwidthInfo{
			MBps:   requestedMBps,
			Mbps:   int(requestedMBps*8 + 0.5),
			Source: LinkSourceRequest,
			Detail: "значение из запроса (оператор)",
		}
	}
	if mbps, id, ok := p.MeasuredLinkMBps(); ok {
		return LinkBandwidthInfo{
			MBps:      float64(mbps) / 8,
			Mbps:      mbps,
			Source:    LinkSourceAuto,
			BackendID: id,
			Detail: "авто-замер балансера до узла " + id +
				" (путь cppworker↔cppworker, как у самих слоёв)",
		}
	}
	if mbps, ok := envLinkMBps(); ok {
		return LinkBandwidthInfo{
			MBps:   float64(mbps) / 8,
			Mbps:   mbps,
			Source: LinkSourceEnv,
			Detail: "LB_RPC_LINK_MBPS из окружения (замер хост↔хост оператором)",
		}
	}
	// Исторический дефолт §3: 1 Гбит/с ≈ 112 МБ/с. Честно помечаем, что это не
	// измерение — на быстром канале оценка будет пессимистичной.
	return LinkBandwidthInfo{
		MBps:   defaultBandwidthMBpsR95,
		Mbps:   int(defaultBandwidthMBpsR95) * 8,
		Source: LinkSourceDefault,
		Detail: "дефолт 1 Гбит/с (замеров нет: ни авто, ни LB_RPC_LINK_MBPS)",
	}
}

// bandwidthBytesPerSecR166 — Б/с для composition.Request через общий резолвер.
func (p *Proxy) bandwidthBytesPerSecR166(requestedMBps float64) float64 {
	return p.LinkBandwidthInfo(requestedMBps).BytesPerSec()
}
