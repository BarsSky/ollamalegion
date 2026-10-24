// stale_model_snapshot_r160.go — R160 (2026-10-17): устаревший снимок «модель
// загружена» не должен превращаться в 502 у клиента.
//
// ЖИВОЙ ДЕФЕКТ (приёмка R157, 2026-10-17). Балансер выбрал донора и проксировал
// запрос, будучи уверенным, что модель там загружена (снимок метрик на тот момент
// был устаревшим — донор пересоздавали). Воркер ответил
//
//	HTTP 500 {"...":"chat generate failed: model gemma-4-E4B-it-Q4_K_M not found"}
//
// а клиент получил от балансера 502 «upstream returned HTTP 500: ...» за 13 мс.
// Это НЕ «плохой шлюз»: балансер знал причину (состояние бэкенда разошлось со
// снимком) и мог её назвать, обновить снимок и запустить загрузку — вместо того
// чтобы отдавать клиенту код, который для OpenAI-клиентов означает «сервер сломан».
//
// ЧТО ДЕЛАЕМ ВМЕСТО 502:
//  1. обновляем снимок метрик этого бэкенда СЕЙЧАС (не ждём тика поллера) —
//     следующая маршрутизация будет идти по факту, а не по вранью;
//  2. запускаем ЗАГРУЗКУ модели там, где она может быть (узел с файлом на диске) —
//     асинхронно, без ожидания на этом запросе;
//  3. отвечаем 503 с Retry-After и объяснением: клиент повторит и получит модель
//     (штатный путь автозагрузки проверен живьём: первый запрос поднимает модель,
//     повтор обслуживается).
//
// ЧЕГО НЕ ДЕЛАЕМ. Не переигрываем запрос «внутри» этого вызова на другой бэкенд:
// автозагрузка живёт в обработчике выше, и такой ретрай ушёл бы на узел без модели
// и вернул ту же ошибку — то есть скрыл бы причину, ничего не починив.
package balancer

import (
	"context"
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// upstreamModelAbsentPhrases — признаки ответа «такой модели у меня нет».
//
// ВАЖНО ПРО ФОРМУ. Живой ответ донора выглядит так:
//
//	chat generate failed: model gemma-4-E4B-it-Q4_K_M not found
//
// то есть между «model» и «not found» стоит ИМЯ модели. Поэтому проверяем не
// соседство слов, а два независимых условия: в теле вообще говорится про model
// и есть один из маркеров отсутствия. Более узкий матч пропускал бы ровно тот
// случай, ради которого всё написано (первая версия так и сделала — поймано
// тестом TestR160_IsUpstreamModelAbsent).
var upstreamModelAbsentPhrases = []string{
	"not found",
	"not_found",
	"not loaded",
	"not_loaded",
}

// isUpstreamModelAbsent — ответ воркера означает «модели, о которой спрашивали,
// у меня нет» (а не «запрос неверный» и не «внутренняя ошибка»).
func isUpstreamModelAbsent(status int, body []byte) bool {
	if status < 500 {
		return false
	}
	lower := strings.ToLower(string(body))
	if !strings.Contains(lower, "model") {
		return false
	}
	for _, m := range upstreamModelAbsentPhrases {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// handleStaleModelSnapshot — ответ клиенту вместо 502 «плохой шлюз».
func (p *Proxy) handleStaleModelSnapshot(w http.ResponseWriter, r *http.Request,
	backendID, model string, upstreamStatus int, upstreamBody []byte) {

	// 1. Снимок был неправдой — обновляем его немедленно.
	if p.llamaCppMetricsPoller != nil {
		if b, ok := p.backendInfoForPoller(backendID); ok {
			p.llamaCppMetricsPoller.pollBackend(b)
		}
	}

	// R229 (2026-10-22): ЕСЛИ МОДЕЛИ НЕТ НИГДЕ — это 404, а не 503.
	//
	// 503 значит «повтори, станет лучше», и клиент (OpenWebUI, Cline, скрипт) уходит
	// в retry-цикл; но файла нет ни на одной машине, и повтор его не создаст.
	// OpenAI-поверхность для этого же случая уже отдавала 404 model_not_found —
	// две поверхности одного стенда обязаны вести себя одинаково.
	//
	// Условие «definitive» защищает от обратной ошибки: каталог по какому-то узлу
	// ещё не опрошен → «файла не видно» не значит «файла нет», остаётся 503.
	// Каталоги, у которых истёк TTL, обновляем прямо здесь (редкий путь ошибки).
	p.refreshStaleModelCatalogs()
	if present, definitive := p.modelPresence(model); definitive && !present {
		available := p.catalogModelNames()
		p.recordModelMissing(backendID, model, upstreamBody)
		logger.Get().Warnw(i18n.T("balancer.stale_model_snapshot.missing_everywhere_404_warn"),
			"backend", backendID, "model", model,
			"available", available, "upstream_body", string(upstreamBody)[:min(300, len(upstreamBody))])
		writeModelMissingResponseFor(w, r.URL.Path, model,
			i18n.T("balancer.stale_model_snapshot.missing_everywhere_reason"), available)
		return
	}

	// 2. Поднимаем модель там, где она есть на диске. Асинхронно: этот запрос
	//    уже не обслужен, а следующий должен застать модель готовой или
	//    загружающейся.
	loadKicked := false
	if p.llamaCppRouter != nil {
		if target := p.llamaCppRouter.selectLlamaCppBackendForModel(model); target != "" {
			go func(bid string) {
				if _, err := p.llamaCppRouter.ensureModelLoadedOnBackend(bid, model,
					warmupOptions{Ctx: context.Background()}); err != nil {
					logger.Get().Debugw(i18n.T("balancer.stale_model_snapshot.load_not_started_debug"),
						"backend", bid, "model", model, "error", err)
				}
			}(target)
			loadKicked = true
		}
	}

	// 3. Честный ответ: 503 + Retry-After. Для OpenAI-клиентов 503 — это
	//    «повтори», для человека — «модель поднимается».
	msg := "backend " + backendID + " no longer has model '" + model +
		"' loaded (stale metric snapshot): snapshot refreshed"
	if loadKicked {
		msg += ", model load started"
	}
	msg += "; retry the request"
	logger.Get().Warnw(i18n.T("balancer.stale_model_snapshot.worker_unknown_model_warn"),
		"backend", backendID, "model", model,
		"upstream_status", upstreamStatus, "load_kicked", loadKicked,
		"upstream_body", string(upstreamBody)[:min(300, len(upstreamBody))])
	writeServiceUnavailable(w, msg, 15)
}
