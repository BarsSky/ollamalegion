// load_provenance_r119.go — R119 (2026-10-15), пункт 4 плана: ПРОИСХОЖДЕНИЕ
// КАЖДОГО параметра загрузки модели (source per-param).
//
// ЗАЧЕМ. У одного параметра может быть четыре источника, и до этой правки
// оператор не мог понять, какое значение реально действует и кто его поставил:
//
//	явное поле запроса клиента        → request
//	per-model профиль (WebUI)         → profile
//	«настройки по умолчанию» (WebUI)  → default_profile
//	env/config контейнера cppworker   → backend (балансер ничего не задавал)
//
// Живая жалоба оператора: «непонятно, к какой модели применяются настройки и
// применяются ли вообще». Профиль мог быть перебит дефолтом, дефолт — env, а
// увидеть это было негде: в UI и API показывались только значения.
//
// КАК СЧИТАЕТСЯ. Слои применяются последовательно (applyProfileLoadParams), и
// каждый заполняет ТОЛЬКО пустые поля. Поэтому provenance ИЗМЕРЯЕТСЯ сравнением
// снимков до/после слоя, а не описывается второй копией правил приоритета:
//
//	что изменилось после профиля модели          → profile
//	что изменилось после «настроек по умолчанию» → default_profile
//	что так и осталось из исходного запроса      → request
//	что осталось пустым                          → backend (решит cppworker)
//	(при ignoreDefaults=true дефолтный профиль не применяется вовсе — это видно
//	 как profile/default_profile/backend, без домыслов)
package balancer

import (
	"reflect"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// Источники значения параметра загрузки.
const (
	LoadSourceRequest        = "request"
	LoadSourceProfile        = "profile"
	LoadSourceDefaultProfile = "default_profile"
	LoadSourceBackend        = "backend"
)

// loadParamOrigin — значение параметра и откуда оно взято.
type loadParamOrigin struct {
	// Value — действующее значение (nil = никто не задавал, решает cppworker).
	Value interface{} `json:"value"`
	// Source — request | profile | default_profile | backend.
	Source string `json:"source"`
}

// loadProvenance — карта «имя параметра → происхождение».
type loadProvenance map[string]loadParamOrigin

// loadProvenanceRecord — запись хранилища: provenance + когда посчитан.
type loadProvenanceRecord struct {
	provenance loadProvenance
	at         time.Time
}

// trackedLoadParams — параметры, по которым показываем происхождение: ровно те,
// что едут в теле загрузки и которые умеет ставить профиль.
func trackedLoadParams(req ModelOpRequest) map[string]interface{} {
	params := map[string]interface{}{
		"contextLength":   nil,
		"batchSize":       nil,
		"gpuLayers":       nil,
		"flashAttn":       nil,
		"useMmap":         nil,
		"kvCacheType":     nil,
		"enableReasoning": nil,
		"parallel":        nil,
		"contextPerSeq":   nil,
	}
	if req.ContextSize != nil {
		params["contextLength"] = *req.ContextSize
	}
	if req.BatchSize != nil {
		params["batchSize"] = *req.BatchSize
	}
	if req.GPULayers != nil {
		params["gpuLayers"] = *req.GPULayers
	}
	if req.FlashAttn != nil {
		params["flashAttn"] = *req.FlashAttn
	}
	if req.UseMmap != nil {
		params["useMmap"] = *req.UseMmap
	}
	if req.KVCacheType != nil {
		params["kvCacheType"] = *req.KVCacheType
	}
	if req.EnableReasoning != nil {
		params["enableReasoning"] = *req.EnableReasoning
	}
	if req.Parallel != nil {
		params["parallel"] = *req.Parallel
	}
	if req.ContextPerSeq != nil {
		params["contextPerSeq"] = *req.ContextPerSeq
	}
	return params
}

// loadProvenanceBuilder — накопитель: держит указатель на живой запрос загрузки
// (его меняют слои) и записывает, какой слой что заполнил.
type loadProvenanceBuilder struct {
	req  *ModelOpRequest
	prov loadProvenance
}

func newLoadProvenanceBuilder(req *ModelOpRequest) *loadProvenanceBuilder {
	return &loadProvenanceBuilder{req: req, prov: loadProvenance{}}
}

// observe — вызвать слой и записать, какие параметры он заполнил.
//
// source — имя слоя (profile / default_profile). Уже отмеченное значение не
// перезаписывается: слои применяются в порядке приоритета, и первый, кто заполнил
// поле, и есть источник истины.
func (b *loadProvenanceBuilder) observe(source string, apply func()) {
	if b == nil || b.req == nil {
		return
	}
	before := trackedLoadParams(*b.req)
	apply()
	after := trackedLoadParams(*b.req)
	for key, val := range after {
		if val == nil || reflect.DeepEqual(before[key], val) {
			continue
		}
		if _, seen := b.prov[key]; seen {
			continue
		}
		b.prov[key] = loadParamOrigin{Value: val, Source: source}
	}
}

// finalize — дополнить карту: что стоит в запросе сейчас и не отмечено слоями —
// request (значение пришло от клиента/WebUI-формы), что пусто — backend
// (cppworker возьмёт свой env/config-дефолт).
func (b *loadProvenanceBuilder) finalize() loadProvenance {
	if b == nil || b.req == nil {
		return nil
	}
	for key, val := range trackedLoadParams(*b.req) {
		if _, seen := b.prov[key]; seen {
			continue
		}
		if val != nil {
			b.prov[key] = loadParamOrigin{Value: val, Source: LoadSourceRequest}
			continue
		}
		b.prov[key] = loadParamOrigin{Value: nil, Source: LoadSourceBackend}
	}
	return b.prov
}

// RecordLoadProvenance — сохранить происхождение параметров загрузки для
// (бэкенд, модель). Экспортирован для тестов и диагностики; боевой вызов —
// из executeLlamaCppLoadOnce.
func (p *Proxy) RecordLoadProvenance(backendID, modelName string, prov loadProvenance) {
	if p == nil || backendID == "" || modelName == "" || len(prov) == 0 {
		return
	}
	key := modelMissingKey(backendID, modelName) // backendID|model (нижний регистр)
	p.mu.Lock()
	if p.loadProvenance == nil {
		p.loadProvenance = make(map[string]loadProvenanceRecord)
	}
	p.loadProvenance[key] = loadProvenanceRecord{provenance: prov, at: time.Now()}
	p.mu.Unlock()
}

// LoadProvenance — происхождение параметров для (бэкенд, модель).
func (p *Proxy) LoadProvenance(backendID, modelName string) loadProvenance {
	if p == nil || backendID == "" || modelName == "" {
		return nil
	}
	key := modelMissingKey(backendID, modelName)
	p.mu.RLock()
	rec, ok := p.loadProvenance[key]
	p.mu.RUnlock()
	if !ok {
		return nil
	}
	return rec.provenance
}

// LlamaCppMetricsWithProvenance — КОПИЯ метрик бэкенда с приписанным происхождением
// параметров загрузки (R119, пункт 4).
//
// ПОЧЕМУ КОПИЯ, а не GetLlamaCppMetrics: тот отдаёт ЖИВОЙ указатель на снапшот
// поллера, и дописывать в него `parameters` из слоя API нельзя — это гонка с
// поллером, который тот же slice перезаписывает. SnapshotLlamaCppMetrics
// возвращает копию (включая копию LoadedModels), в неё provenance писать безопасно.
func (p *Proxy) LlamaCppMetricsWithProvenance(backendID string) *types.LlamaCppMetrics {
	if p == nil || p.metricsMgr == nil || backendID == "" {
		return nil
	}
	lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics(backendID)
	if !ok || lm == nil {
		return nil
	}
	p.attachProvenance(backendID, lm)
	return lm
}

// attachProvenance — приписать происхождение параметров каждой загруженной модели
// из реестра. Общий помощник: используется и cluster state, и API-слоем, чтобы
// источник был виден на всех поверхностях одинаково.
func (p *Proxy) attachProvenance(backendID string, lm *types.LlamaCppMetrics) {
	if p == nil || lm == nil || len(lm.LoadedModels) == 0 {
		return
	}
	for i := range lm.LoadedModels {
		if prov := p.LoadProvenanceJSON(backendID, lm.LoadedModels[i].Name); len(prov) > 0 {
			lm.LoadedModels[i].Parameters = prov
		}
	}
}

// LoadProvenanceJSON — provenance в виде, готовом для JSON-ответа API:
//
//	{"contextLength":{"value":8192,"source":"profile"},…}
//
// Нужен API-слою: WebUI рисует источник рядом со значением
// (см. internal/api/gguf_backends_handler.go, поле parameters).
func (p *Proxy) LoadProvenanceJSON(backendID, modelName string) map[string]interface{} {
	prov := p.LoadProvenance(backendID, modelName)
	if len(prov) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(prov))
	for k, v := range prov {
		out[k] = map[string]interface{}{"value": v.Value, "source": v.Source}
	}
	return out
}
