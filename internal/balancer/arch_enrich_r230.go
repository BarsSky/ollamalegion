// arch_enrich_r230.go — Round 230 (2026-10-24): параметры архитектуры модели
// для расчёта KV cache.
//
// ЗАЧЕМ. cppworker в `/api/models` отдаёт n_layers/n_embd, но НЕ отдаёт
// n_kv_heads/head_dim_k (проверено живьём: загруженная gemma-4-E4B-it-Q4_K_M,
// nLayers=42, nEmbd=2560, а nKvHeads/headDimK пустые). Балансер без этих двух
// чисел физически не может посчитать размер KV cache, поэтому AutoTune:
//   - не мог сравнить KV текущего n_ctx с бюджетом (в R229 это вылилось в
//     «свободно 0 GB» и рекомендации q4_0/-2, см. autotune_budget_r230.go);
//   - вынужден был угадывать по размеру модели (R54.1 heuristic).
//
// cppworker УЖЕ отдаёт эти поля в ответе `/api/show` для загруженной модели
// (writeOllamaShowLoadedResponse: model_info.n_kv_heads, head_dim_k,
// head_dim_v, n_embd). Здесь балансер добирает их из /api/show ОДИН раз на
// пару (бэкенд, модель): параметры GGUF-файла не меняются между перезагрузками,
// а лишний HTTP-запрос на каждом тике поллера не нужен.
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// archParams — то, чего не хватает в /api/models для расчёта KV cache.
type archParams struct {
	NKvHeads int
	NHeads   int
	HeadDimK int
	HeadDimV int
	NEmbd    int
}

// usable — данных достаточно, чтобы посчитать KV cache.
func (a archParams) usable() bool { return a.NKvHeads > 0 && a.HeadDimK > 0 }

// derive заполняет отсутствующее из известного по соглашениям llama.cpp:
//   - head_dim = n_embd / n_heads (n_embd_head_k);
//   - n_kv_heads = n_heads (MHA), тот же fallback, что у cppworker (auto_offload.go).
func (a *archParams) derive() {
	if a.NEmbd > 0 && a.NHeads > 0 {
		if a.HeadDimK == 0 {
			a.HeadDimK = a.NEmbd / a.NHeads
		}
		if a.HeadDimV == 0 {
			a.HeadDimV = a.HeadDimK
		}
	}
	if a.NKvHeads == 0 {
		a.NKvHeads = a.NHeads
	}
}

// enrichLoadedModelsArch добирает параметры архитектуры для loaded-моделей,
// у которых их нет. Best-effort: неудача (старый cppworker, нет ответа) просто
// оставляет поля пустыми — AutoTune тогда консервативно не меняет раскладку.
func (p *llamaCppMetricsPoller) enrichLoadedModelsArch(b backendInfo, models []types.LlamaCppModel) []types.LlamaCppModel {
	for i := range models {
		m := &models[i]
		if m.State != "loaded" || m.ContextLength <= 0 {
			continue
		}
		if m.NKvHeads > 0 && m.HeadDimK > 0 {
			continue // cppworker уже прислал (новая версия /api/models)
		}
		key := b.id + "|" + m.Name
		if cached, ok := p.cachedArch(key); ok {
			applyArchParams(m, cached)
			continue
		}
		arch, ok := p.fetchArchParams(b, m.Name)
		if !ok {
			continue
		}
		p.storeArch(key, arch)
		applyArchParams(m, arch)
	}
	return models
}

// applyArchParams заполняет только пустые поля: явные значения воркера
// авторитетнее добранных.
func applyArchParams(m *types.LlamaCppModel, a archParams) {
	if m.NKvHeads == 0 {
		m.NKvHeads = a.NKvHeads
	}
	if m.HeadDimK == 0 {
		m.HeadDimK = a.HeadDimK
	}
	if m.HeadDimV == 0 {
		m.HeadDimV = a.HeadDimV
	}
	if m.NEmbd == 0 {
		m.NEmbd = a.NEmbd
	}
}

func (p *llamaCppMetricsPoller) cachedArch(key string) (archParams, bool) {
	p.archMu.Lock()
	defer p.archMu.Unlock()
	if p.archCache == nil {
		return archParams{}, false
	}
	a, ok := p.archCache[key]
	return a, ok
}

func (p *llamaCppMetricsPoller) storeArch(key string, a archParams) {
	p.archMu.Lock()
	defer p.archMu.Unlock()
	if p.archCache == nil {
		p.archCache = make(map[string]archParams)
	}
	p.archCache[key] = a
}

// fetchArchParams — POST /api/show {"name": "..."} и разбор model_info.
func (p *llamaCppMetricsPoller) fetchArchParams(b backendInfo, name string) (archParams, bool) {
	url := fmt.Sprintf("http://%s:%d/api/show", b.host, b.port)
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return archParams{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return archParams{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return archParams{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return archParams{}, false
	}
	var data struct {
		ModelInfo map[string]interface{} `json:"model_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return archParams{}, false
	}
	arch := archParams{
		NKvHeads: firstIntField(data.ModelInfo, "n_kv_heads", "general.n_kv_heads"),
		NHeads:   firstIntField(data.ModelInfo, "n_heads", "general.n_heads"),
		HeadDimK: firstIntField(data.ModelInfo, "head_dim_k", "general.head_dim_k", "n_embd_head_k"),
		HeadDimV: firstIntField(data.ModelInfo, "head_dim_v", "general.head_dim_v", "n_embd_head_v"),
		NEmbd:    firstIntField(data.ModelInfo, "n_embd", "general.n_embd"),
	}
	arch.derive()
	if !arch.usable() {
		return archParams{}, false
	}
	return arch, true
}

// firstIntField — первое числовое значение из списка ключей (json-числа
// приходят как float64, но допускаем int/строку — ответы бывают разными).
func firstIntField(m map[string]interface{}, keys ...string) int {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case float64:
			return int(t)
		case int:
			return t
		case int64:
			return int(t)
		case string:
			if n, err := strconv.Atoi(t); err == nil {
				return n
			}
		}
	}
	return 0
}
