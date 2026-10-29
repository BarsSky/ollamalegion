package api

// handlers_llama_version_r269.go — R269 (2026-10-29): GET /api/v1/llama-version.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ЭНДПОИНТ, ЕСЛИ ЕСТЬ ПОЛЕ В /api/v1/backends. Уведомление
// «есть более свежая стабильная версия llama.cpp» — это состояние КЛАСТЕРА, а
// не отдельного узла: WebUI должен уметь спросить его одним запросом, не
// собирая список бэкендов (и не завися от того, попал ли узел в отфильтрованный
// список). Плюс здесь видно, ВКЛЮЧЕНА ли проверка и была ли последняя ошибка
// сети — иначе «уведомления нет» неотличимо от «проверка молчит».
//
// Контракт только добавляет данные: ни одно прежнее поле не меняется.

import (
	"net/http"
)

// llamaVersionHandler — GET /api/v1/llama-version.
func (s *Server) llamaVersionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s == nil || s.proxy == nil {
		http.Error(w, `{"error":"proxy not available"}`, http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, http.StatusOK, s.proxy.LlamaUpstreamSnapshot())
}
