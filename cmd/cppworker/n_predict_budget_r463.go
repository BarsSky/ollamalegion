// n_predict_budget_r463.go — R463/R464 (2026-11-06): бюджет вывода, когда
// клиент задал только ВЕРХНЮЮ ГРАНИЦУ (`max_output_tokens`), и честный признак
// «клиент сам ограничил длину».
//
// ЖИВОЙ ДЕФЕКТ (второй стенд: A10 ×2, 50 ГБ, Qwen3.8-27B). Клиент (Cline) не
// присылает `num_predict` — присылает `max_output_tokens` (например 27392) как
// «сколько ответа вообще приму». Политика R411 подставляла дефолт reasoning-модели
// 8192, а граница клиента могла только УМЕНЬШАТЬ. На 27B размышления съедали все
// 8192 → `done_reason=length`, видимого ответа нет → клиент запускал свою
// overflow-recovery (compacting → compacted) и падал с «text part … not found»,
// теряя часть переписки. В логе при этом появлялось «R188 повтор ограничен …
// клиент задал длину ответа n_predict=8192», хотя клиент длину НЕ задавал.
//
// ЧТО МЕНЯЕМ (два независимых правила):
//
//  1. R463: если клиент НЕ задал явную длину, но задал верхнюю границу — бюджет
//     берётся как max(дефолт модели, min(граница, потолок)). То есть граница
//     теперь может и ПОДНЯТЬ дефолт: клиент сам сказал, сколько готов принять, и
//     это не догадка балансера. Потолок (CPPWORKER_MAX_N_PREDICT, по умолчанию
//     16384) — предохранитель от «разрешить 27k токенов на 27B при CPU-оффлоаде».
//  2. R464: признак «клиент ограничил длину» — это ЯВНО заданный num_predict/
//     max_tokens, а не наш вычисленный бюджет. От него зависит бюджет повторов
//     (R188) и текст лога; раньше подставленный нами же дефолт выдавался за
//     клиентское ограничение.
package main

import (
	"os"
	"strconv"
	"strings"
)

// maxNPredictCeilingR463 — верхний предел бюджета вывода, который мы готовы
// разрешить по клиентской границе.
//
// Зачем потолок: `max_output_tokens` у агентских клиентов часто «на всякий
// случай» (32k–64k). Разрешать их целиком на 27B с CPU-оффлоадом — десятки минут
// на ход. 16384 — компромисс: хватает на размышления + полноценный ответ с
// инструментами, но не превращает один ход в полчаса.
const defaultMaxNPredictR463 = 16384

// maxNPredictCeilingR463 — потолок из env (0/мусор = дефолт).
func maxNPredictCeilingR463() int {
	raw := strings.TrimSpace(os.Getenv("CPPWORKER_MAX_N_PREDICT"))
	if raw == "" {
		return defaultMaxNPredictR463
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultMaxNPredictR463
	}
	return v
}

// resolveOutputBudgetR463 — итоговый бюджет вывода и его источник.
//
// Источники (в порядке приоритета):
//   - "client_num_predict" — клиент задал явную длину (num_predict / max_tokens);
//   - "client_upper_bound" — клиент задал только max_output_tokens, и он больше
//     дефолта модели (R463: граница поднимает дефолт);
//   - "model_default" — ни того, ни другого: дефолт политики (reasoning-модели
//     получают DefaultNPredictReasoning, остальные — дефолт моста).
//
// Потолок maxNPredictCeilingR463 применяется к любому источнику: он ограничивает
// только то, что мы сами разрешили по границе, поэтому клиентский явный
// num_predict тоже упирается в него (иначе клиент мог бы попросить 65536 и
// получить многочасовой ход).
func resolveOutputBudgetR463(clientNumPredict, clientUpperBound int, modelName string) (budget int, source string) {
	if clientNumPredict > 0 {
		budget, source = clientNumPredict, "client_num_predict"
	} else {
		budget = ResolveNPredict(0, modelName)
		source = "model_default"
		if clientUpperBound > budget {
			budget, source = clientUpperBound, "client_upper_bound"
		}
	}
	if ceiling := maxNPredictCeilingR463(); ceiling > 0 && budget > ceiling {
		budget = ceiling
		source += "+ceiling"
	}
	return budget, source
}

// clientSetOutputLimit — клиент ЯВНО ограничил длину ответа (R464).
//
// Только это даёт право считать обрезанный ход ожидаемым результатом клиентского
// лимита (один повтор вместо полного бюджета, текст отказа про лимит клиента).
// Наш собственный дефолт (8192 для reasoning) таким ограничением не является.
func clientSetOutputLimit(clientNumPredict int) bool {
	return clientNumPredict > 0
}

// clientSetOutputLimitChat — то же для /api/chat, где max_tokens приходит
// указателем (nil = «поле не задано», explicit 0 = «дай сколько дашь»), а
// num_predict живёт в options.
func clientSetOutputLimitChat(maxTokens *int, optionsNumPredict int) bool {
	if maxTokens != nil && *maxTokens > 0 {
		return true
	}
	return optionsNumPredict > 0
}
