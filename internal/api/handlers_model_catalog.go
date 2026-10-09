// handlers_model_catalog.go — R91 (2026-10-09): read-only каталог моделей НА
// ДИСКЕ по бэкендам.
//
//	GET /api/v1/models/catalog            — снимок каталога (файлы + свежесть)
//	GET /api/v1/models/catalog?refresh=true — перечитать листинги у воркеров
//
// ЗАЧЕМ ОТДЕЛЬНАЯ РУЧКА. Балансер выбирает узел для незагруженной модели по
// каталогу (internal/balancer/model_catalog.go), и когда запрос всё-таки падает
// с model_not_found, первый вопрос оператора — «а что балансер знал про файлы на
// узлах и насколько свежо». Здесь этот срез виден как есть, вместе с возрастом
// снимка, поэтому не нужно лазить по логам и по самим воркерам.
package api

import (
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// handleModelsCatalog — GET /api/v1/models/catalog.
func (s *Server) handleModelsCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET"})
		return
	}
	if s.proxy == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "proxy is not available"})
		return
	}

	// ?refresh=true — явный запрос оператора: обновляем независимо от
	// LB_MODEL_CATALOG_REFRESH_SEC (в т.ч. когда фоновое обновление выключено).
	refreshed := false
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("refresh")), "true") {
		s.proxy.RefreshModelCatalogNow()
		refreshed = true
	}

	snapshot := s.proxy.ModelCatalogSnapshot()

	// Какие бэкенды каталог вообще покрывает: llama.cpp-тип и не offline.
	// Записи без снимка показываем явно — «не опрошен» и «пусто на диске» разные
	// вещи, и в диагностике это первое, что нужно различить.
	// Порядок полей — по требованию govet fieldalignment (govet enable-all в
	// .golangci.yml): сначала строки, затем срез (у него хвост len/cap —
	// не указатели, поэтому префикс указателей короче), затем скаляры.
	type backendCatalogView struct {
		BackendID string   `json:"backendId"`
		Type      string   `json:"type"`
		Status    string   `json:"status"`
		Host      string   `json:"host"`
		Note      string   `json:"note,omitempty"`
		Files     []string `json:"files,omitempty"`
		Known     bool     `json:"known"`
		// AgeSec без omitempty: 0 — это «снимок только что снят», и он должен быть
		// виден в ответе (иначе свежий каталог выглядел бы как отсутствующий).
		AgeSec int `json:"ageSec"`
	}

	rows := make([]backendCatalogView, 0)
	for _, b := range s.proxy.GetAllBackends() {
		isLlama := b.Type == types.BackendTypeLlamaCpp
		entry, hasEntry := snapshot[b.ID]
		row := backendCatalogView{
			BackendID: b.ID,
			Type:      string(b.Type),
			Status:    string(b.Status),
			Host:      b.Host,
			Known:     hasEntry,
		}
		if hasEntry {
			row.Files = entry.Files
			row.AgeSec = entry.AgeSec
		} else if isLlama {
			row.Note = "каталог этого бэкенда ещё не опрошен: выбор для незагруженной модели работает как раньше (любой healthy узел)"
		} else {
			row.Note = "каталог ведётся только для llama.cpp-бэкендов (у них есть /api/models/files)"
		}
		rows = append(rows, row)
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends":  rows,
		"total":     len(rows),
		"refreshed": refreshed,
		"note": "каталог — кэш листингов /api/models/files; «known» = снимок есть, " +
			"пустой список файлов при known=true означает, что моделей на диске нет",
	})
}
