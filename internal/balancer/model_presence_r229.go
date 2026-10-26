package balancer

import (
	"sort"

	"ollama-loadbalancer/pkg/types"
)

// model_presence_r229.go — R229 (2026-10-22): «модели нет нигде» — это 404, а не 503.
//
// ЖИВОЙ ДЕФЕКТ (стенд, до правки): запрос на неизвестную модель через нативную
// Ollama-поверхность получал
//
//	503 {"error":"backend … no longer has model 'no-such-model-xyz' loaded
//	(stale metric snapshot): snapshot refreshed …; retry the request"}
//
// Но воркер ответил «model … not found», модели НЕТ НИ НА ОДНОЙ машине, и повтор
// её не создаст: 503 означает «повтори, станет лучше» и уводит клиента (OpenWebUI,
// Cline, скрипты) в retry-цикл. OpenAI-поверхность для того же случая уже отвечала
// 404 model_not_found, то есть две поверхности одного стенда вели себя по-разному.
//
// ПОЧЕМУ НЕЛЬЗЯ ОТВЕЧАТЬ 404 ВСЕГДА. Каталог моделей опрашивается по бэкендам и
// может быть неполным (воркер только что поднялся, каталог ещё не собран). Тогда
// «файла не видно» не значит «файла нет» — в этом случае оставляем 503 с повтором,
// как раньше. 404 отдаём только когда каталог опрошен по ВСЕМ llama.cpp-бэкендам и
// файла нет нигде: это утверждение, а не догадка.

// modelPresence — есть ли модель на диске хоть у одного llama.cpp-бэкенда и можно
// ли этому верить.
//
// present=true — файл найден (или модель уже загружена: тогда файл точно есть).
// definitive=true — каталог опрошен по всем llama.cpp-бэкендам, поэтому отсутствие
// файла — факт, а не отсутствие телеметрии.
func (p *Proxy) modelPresence(model string) (present, definitive bool) {
	if p == nil || model == "" {
		return false, false
	}
	backends := p.GetAllBackends()
	llamaBackends := 0
	unknown := 0
	for i := range backends {
		b := &backends[i]
		if normalizeBackendType(b.Type) != types.BackendTypeLlamaCpp {
			continue
		}
		llamaBackends++
		if b.ID != "" {
			if lm := p.llamaMetricsForPresence(b.ID); lm != nil {
				for _, m := range lm.LoadedModels {
					if modelNameMatches(m.Name, model) {
						return true, true
					}
				}
			}
		}
		has, known := p.BackendModelOnDisk(b.ID, model)
		if has {
			return true, true
		}
		if !known {
			unknown++
		}
	}
	if llamaBackends == 0 || unknown > 0 {
		return false, false
	}
	return false, true
}

// llamaMetricsForPresence — загруженные модели бэкенда (nil-safe).
// R245: снапшот под RLock; живой указатель читатели разыменовывали после
// разблокировки, пока поллер переписывал поля (data race, ловится -race в CI).
func (p *Proxy) llamaMetricsForPresence(backendID string) *types.LlamaCppMetrics {
	if p == nil || p.metricsMgr == nil || backendID == "" {
		return nil
	}
	lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics(backendID)
	if !ok {
		return nil
	}
	return lm
}

// catalogModelNames — объединение файлов моделей по всем бэкендам (для
// availableModels в 404-ответе). Отсортировано и без дублей.
func (p *Proxy) catalogModelNames() []string {
	if p == nil {
		return nil
	}
	snap := p.ModelCatalogSnapshot()
	seen := map[string]struct{}{}
	out := make([]string, 0, len(snap))
	for _, s := range snap {
		for _, name := range s.Files {
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
