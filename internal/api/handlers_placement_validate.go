package api

// R95 (S4 плана 2026-10-07): POST /api/v1/placement/validate — сухой прогон
// композиции.
//
// ЧТО ЭТО. Оператор (или wizard S5, или окно объединения S6) присылает
// ПРЕДЛАГАЕМЫЙ состав кластера и параметры модели, а в ответ получает список
// замечаний §7 с числами — до того, как что-либо сохранено.
//
// ПОЧЕМУ POST, А НЕ GET. Проверяется КОМПОЗИЦИЯ, которой может ещё не быть в
// конфиге: её состав (роли, RPC-эндпоинты, стратегия) не влезает в query-строку
// без потери читаемости, а GET с телом — плохая практика.
//
// ЭНДПОИНТ НИЧЕГО НЕ МЕНЯЕТ. Это dry-run: правило стенда — «WebUI только
// предлагает». Применение политики и групп — отдельные операции конфига.

import (
	"encoding/json"
	"net/http"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/composition"
	"ollama-loadbalancer/internal/i18n"
)

func (s *Server) placementValidateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"error":   i18n.T("api.handlers_placement_validate.method_post_only"),
		})
		return
	}

	// 1 МиБ с запасом: состав кластера — это сотни байт, а не мегабайты.
	var req balancer.CompositionRequestR95
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   i18n.T("api.handlers_placement_validate.bad_request_body", err),
		})
		return
	}
	if len(req.Group.Members) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error": i18n.T("api.handlers_placement_validate.group_required") + " " +
				`{"group":{"id":"g1","model":"qwen3-14b","strategy":"layer","members":[{"backendId":"a","role":"master"},{"backendId":"b","role":"worker","rpcEndpoint":"host:50052"}]}}`,
		})
		return
	}

	report := s.proxy.ValidateCompositionR95(req)
	// R166: честно показываем, какая пропускная способность ушла в оценку трафика
	// и откуда она взялась (запрос / автозамер / env / дефолт).
	bandwidth := s.proxy.LinkBandwidthInfo(req.BandwidthMBps)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"dryRun":    true,
		"lang":      report.Lang,
		"groupId":   report.GroupID,
		"model":     report.Model,
		"hasErrors": report.HasErrors(),
		// Разложено по уровням для UI: красное/жёлтое/подсказка.
		"errors":   report.Errors(),
		"warnings": report.Warnings(),
		"hints":    report.Hints(),
		// issues — тот же список, но нормализованный: без этого при отсутствии
		// замечаний уходил `null`, тогда как errors/warnings/hints всегда массивы.
		// Фронтенд от `null` защищён (issuesHtml проверяет !issues), а вот
		// диагностические скрипты и сторонние клиенты спотыкаются: в PowerShell
		// @($null).Count равен 1, то есть «одно пустое замечание» вместо «ни одного».
		"issues": issuesOrEmptyR95(report.Issues),
		// R166: какая пропускная способность канала ушла в оценку трафика и откуда
		// она взялась (request | auto | env | default) — ключевой параметр раскладки.
		"bandwidth": bandwidth,
		// structural-ошибки конфига группы (нет мастера, воркер без rpcEndpoint)
		// идут отдельным списком: они про конфиг, а issues — про собираемость.
		"configWarnings": warningsOrEmpty(balancer.CompositionConfigWarningsR95(req.Group)),
		// Полный список проверок: UI показывает «проверок 11, ошибок 0», а не
		// пустой блок, когда всё хорошо.
		"checks": composition.AllChecks(),
		"hint":   i18n.T("api.handlers_placement_validate.hint_dry_run"),
	})
}

// issuesOrEmptyR95 — пустой массив вместо null.
//
// Соседние поля (errors/warnings/hints) всегда массивы, потому что их собирают
// через Errors()/Warnings()/Hints() с make(..., 0, n). У issues такого слоя не
// было, и при отсутствии замечаний уходил `null`. На этом спотыкаются не
// браузерные потребители (issuesHtml проверяет !issues), а диагностика: в
// PowerShell @($null).Count == 1, и «замечаний нет» читается как «одно пустое».
func issuesOrEmptyR95(in []composition.Issue) []composition.Issue {
	if in == nil {
		return []composition.Issue{}
	}
	return in
}
