// Package balancer — R95 (S4 плана 2026-10-07): сухой прогон композиции.
//
// План §6 (S4): «Единый валидатор композиции: доступность узлов, версия llama.cpp,
// свободная VRAM по узлам, H·L-оценка трафика (§3), достаточность RAM под KV» +
// «сухой прогон (dry-run) без сохранения».
//
// РАЗДЕЛЕНИЕ ОТВЕТСТВЕННОСТИ. Сами проверки — чистые функции в
// internal/composition (их можно прогонять без балансера и без стенда). Здесь —
// адаптер: он собирает вход из ЖИВОГО состояния (health бэкендов, метрики VRAM и
// RAM, типы, «личность» GPU) и пробует TCP-доступность RPC-портов воркеров.
//
// ПОЧЕМУ ПРОБА ПОРТА ЗДЕСЬ. «Узел X не отвечает на 50052» — единственная
// проверка §7, которую нельзя сделать по метрикам: rpc-server может быть не
// поднят, пока сам cppworker жив и healthy. Проба — DialTimeout без handshake:
// нам нужен факт «порт слушает», а не протокол (его проверит S1 при загрузке).
package balancer

import (
	"net"
	"strings"
	"time"

	"ollama-loadbalancer/internal/composition"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// rpcProbeTimeoutR95 — таймаут пробы RPC-порта.
//
// Короткий намеренно: сухой прогон запускает оператор из UI, и он не должен
// ждать. 800 мс хватает даже на медленный Wi-Fi в той же локальной сети, а
// «connection refused» на живом хосте приходит мгновенно.
const rpcProbeTimeoutR95 = 800 * time.Millisecond

// defaultBandwidthMBpsR95 — пропускная способность по умолчанию для оценки
// трафика: 112 МБ/с — полезная нагрузка 1 Гбит/с из §3 плана (900–940 Мбит/с).
const defaultBandwidthMBpsR95 = 112.0

// CompositionRequestR95 — тело POST /api/v1/placement/validate.
//
// Порядок полей — по требованию fieldalignment.
type CompositionRequestR95 struct {
	// Group — ПРЕДЛАГАЕМАЯ композиция: она может ещё не быть в конфиге, в этом и
	// смысл сухого прогона («проверка до сохранения»).
	Group types.BackendGroupConfig `json:"group"`
	// Model — параметры модели; чего нет — берётся из профиля/метрик балансера.
	Model CompositionModelR95 `json:"model"`
	// Lang — язык текстов замечаний (ru/en).
	Lang string `json:"lang"`
	// BandwidthMBps — измеренная пропускная способность (0 → 112 МБ/с).
	BandwidthMBps float64 `json:"bandwidthMBps"`
	// PrefillTokens — длина префилла для оценки трафика (0 → 512).
	PrefillTokens int `json:"prefillTokens"`
	// TokensPerSecond — скорость декода для доли времени токена (0 → не считать).
	TokensPerSecond float64 `json:"tokensPerSecond"`
	// RequireHomogeneous — правило auto.requireHomogeneous.
	RequireHomogeneous bool `json:"requireHomogeneous"`
	// SkipProbes — не пробовать RPC-порты (тесты, заведомо недоступная сеть).
	SkipProbes bool `json:"skipProbes"`
}

// CompositionModelR95 — параметры модели в запросе сухого прогона.
//
// Порядок полей — по требованию fieldalignment.
type CompositionModelR95 struct {
	Name           string `json:"name"`
	BackendType    string `json:"backendType"`
	SizeBytes      int64  `json:"sizeBytes"`
	HiddenSize     int    `json:"hiddenSize"`
	Layers         int    `json:"layers"`
	NCtx           int    `json:"nCtx"`
	KVLayers       int    `json:"kvLayers"`
	KVHeads        int    `json:"kvHeads"`
	KVHeadDim      int    `json:"kvHeadDim"`
	KVBytesPerElem int    `json:"kvBytesPerElem"`
	BytesPerElem   int    `json:"bytesPerElem"`
}

// ValidateCompositionR95 прогоняет проверки §7 на живых данных.
//
// Ничего не сохраняет и не меняет: это dry-run (правило стенда —
// «WebUI только предлагает»).
func (p *Proxy) ValidateCompositionR95(req CompositionRequestR95) composition.Report {
	if p == nil {
		return composition.Report{
			Lang: composition.NormalizeLang(req.Lang),
			Issues: []composition.Issue{{
				Check: composition.CheckNodeUnreachable,
				Level: composition.LevelError,
				Text:  "балансер недоступен",
			}},
		}
	}

	report := composition.Validate(composition.Request{
		Group:                p.compositionGroupR95(req),
		Model:                p.compositionModelR95(req),
		Lang:                 req.Lang,
		PrefillTokens:        req.PrefillTokens,
		TokensPerSecond:      req.TokensPerSecond,
		BandwidthBytesPerSec: p.bandwidthBytesPerSecR166(req.BandwidthMBps),
	})

	logger.Get().Infow("composition dry-run",
		"group", req.Group.ID, "model", report.Model, "strategy", req.Group.Strategy,
		"issues", len(report.Issues), "errors", len(report.Errors()),
		"warnings", len(report.Warnings()), "has_errors", report.HasErrors())
	return report
}

// CompositionConfigWarningsR95 — структурные ошибки конфига групп (тексты
// русского валидатора types.ValidateBackendGroups).
//
// Отдаются ОТДЕЛЬНО от issues: они про конфиг (нет мастера, воркер без endpoint),
// а issues — про собираемость раскладки. Слить их в один локализованный список —
// задача S5 (там же, где UI начнёт рисовать подписи по CheckID).
func CompositionConfigWarningsR95(cfg types.BackendGroupConfig) []string {
	if strings.TrimSpace(cfg.ID) == "" && len(cfg.Members) == 0 {
		return nil
	}
	return types.ValidateBackendGroups([]types.BackendGroupConfig{cfg})
}

// compositionGroupR95 собирает описываемую группу.
func (p *Proxy) compositionGroupR95(req CompositionRequestR95) composition.Group {
	group := composition.Group{
		ID:                 strings.TrimSpace(req.Group.ID),
		Model:              strings.TrimSpace(req.Group.Model),
		Strategy:           composition.NormalizeStrategy(req.Group.Strategy),
		RequireHomogeneous: req.RequireHomogeneous,
		Nodes:              make([]composition.Node, 0, len(req.Group.Members)),
	}
	for _, member := range req.Group.Members {
		group.Nodes = append(group.Nodes, p.compositionNodeR95(member, req.SkipProbes))
	}
	return group
}

// compositionModelR95 собирает модель: явные поля запроса важнее профиля.
func (p *Proxy) compositionModelR95(req CompositionRequestR95) composition.Model {
	model := composition.Model{
		Name:           strings.TrimSpace(req.Model.Name),
		SizeBytes:      req.Model.SizeBytes,
		HiddenSize:     req.Model.HiddenSize,
		Layers:         req.Model.Layers,
		NCtx:           req.Model.NCtx,
		KVLayers:       req.Model.KVLayers,
		KVHeads:        req.Model.KVHeads,
		KVHeadDim:      req.Model.KVHeadDim,
		KVBytesPerElem: req.Model.KVBytesPerElem,
		BytesPerElem:   req.Model.BytesPerElem,
	}
	if model.Name == "" {
		// Имя модели в запросе не обязательно: маска группы его и подразумевает.
		model.Name = strings.TrimSpace(req.Group.Model)
	}
	if strings.TrimSpace(req.Model.BackendType) != "" {
		// Пустой тип НЕ нормируем в "ollama": это значило бы «запрос требует
		// ollama» и дало бы ложную ошибку на llama.cpp-узлах.
		model.BackendType = composition.NormalizeBackendType(req.Model.BackendType)
	}
	if model.BytesPerElem <= 0 {
		model.BytesPerElem = 2
	}
	if model.SizeBytes <= 0 {
		// Размер не задали — берём из профиля/метрик балансера (ScanModelsIfStale
		// уже умеет читать GGUF-размеры).
		if size := p.getModelSizeBytes(model.Name); size > 0 {
			model.SizeBytes = size
		}
	}
	model.SizeKnown = model.SizeBytes > 0
	return model
}

// compositionNodeR95 собирает узел композиции из живого состояния балансера.
func (p *Proxy) compositionNodeR95(member types.BackendGroupMemberConfig, skipProbes bool) composition.Node {
	backendID := strings.TrimSpace(member.BackendID)
	role := types.ParseBackendGroupRole(member.Role)
	node := composition.Node{
		BackendID:   backendID,
		Role:        role,
		RPCEndpoint: strings.TrimSpace(member.RPCEndpoint),
		GPUIdentity: p.backendGPUIdentity(backendID),
	}

	info := p.groupMemberInfoR92(backendID)
	node.Registered = info.Registered
	node.Healthy = info.Healthy
	node.Status = info.Status
	node.VRAMKnown = info.VRAMKnown
	node.VRAMFreeMB = info.VRAMFreeMB
	node.VRAMTotalMB = info.VRAMTotalMB

	p.mu.RLock()
	state := p.backends[backendID]
	p.mu.RUnlock()
	if state != nil && state.Backend != nil {
		node.BackendType = string(normalizeBackendType(state.Backend.Type))
	}

	if p.metricsMgr != nil {
		if metrics, ok := p.metricsMgr.SnapshotBackendMetrics(backendID); ok {
			node.RAMFreeMB = int(metrics.System.MemoryFree)
			node.RAMKnown = metrics.System.MemoryTotal > 0
		}
	}

	// Версия llama.cpp узла (R101): приходит при регистрации/heartbeat
	// (env `CPPWORKER_LLAMA_BUILD` → `types.Backend.BuildInfo`), см. handlers_backends.go.
	//
	// Пустая строка — узел версию не сообщает: валидатор честно помечает
	// совместимость сторон RPC неподтверждённой (warning `version_unknown`), а не
	// считает проверку пройденной. Выдумывать версию нельзя: на ней строится
	// вывод «можно ли соединять эти два узла».
	if state != nil && state.Backend != nil {
		node.LlamaCppVersion = strings.TrimSpace(state.Backend.BuildInfo)
	}

	if role == types.BackendGroupRoleWorker && node.RPCEndpoint != "" && !skipProbes {
		node.RPCChecked = true
		node.RPCReachable, node.RPCError = probeRPCEndpointR95(node.RPCEndpoint)
	}
	return node
}

// probeRPCEndpointR95 — слушает ли RPC-порт.
func probeRPCEndpointR95(endpoint string) (bool, string) {
	conn, err := net.DialTimeout("tcp", endpoint, rpcProbeTimeoutR95)
	if err != nil {
		return false, err.Error()
	}
	_ = conn.Close()
	return true, ""
}

// bandwidthBytesPerSecR95 — Б/с из запроса (МБ/с) или дефолт §3.
func bandwidthBytesPerSecR95(mbPerSec float64) float64 {
	if mbPerSec <= 0 {
		mbPerSec = defaultBandwidthMBpsR95
	}
	return mbPerSec * 1000 * 1000
}
