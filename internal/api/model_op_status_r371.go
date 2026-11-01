// model_op_status_r371.go — R371 (2026-11-01): отказ по РОЛИ бэкенда
// (донор раскладки не поднимает своих копий) — это 409, а не 500.
//
// ЖИВОЙ ФАКТ (двухузловой стенд, проверка R304):
//
//	POST /api/v1/cluster/models/gemma-4-E4B-it-Q4_K_M/reload {"backendId":"cppworker-donor"}
//	→ 200 {"results":[{"backendId":"cppworker-donor","status":"error",
//	                   "error":"узел cppworker-donor — донор раскладки: он отдаёт VRAM
//	                            мастеру и не поднимает своих копий моделей",
//	                   "httpStatus":500}]}
//
// Текст правильный (R304 отработал, копия на доноре не поднялась — проверено
// `GET /api/models` донора: моделей нет), а КОД врёт: 500 — это «сервер
// сломался», хотя ничего не сломалось, запрос просто противоречит роли узла.
// Клиент/WebUI по 5xx идёт в «повторите/сообщите о сбое», а повторять здесь
// нечего.
//
// Поэтому решение о коде вынесено в одну функцию: загрузка, запрещённая ролью
// (ModelOpResult.RoleRefused), отвечает 409 Conflict и несёт `roleRefused:true`,
// чтобы UI мог показать «выбран донор раскладки» без разбора текста.
package api

import (
	"net/http"

	"ollama-loadbalancer/internal/balancer"
)

// opFailureStatusR371 — HTTP-код для неуспешной операции с моделью.
//
// Всё, что мы не классифицировали, остаётся 500: молча превращать любую ошибку
// в 4xx нельзя — тогда реальные сбои воркера перестанут выглядеть сбоями.
func opFailureStatusR371(opResult *balancer.ModelOpResult) int {
	if opResult != nil && opResult.RoleRefused {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}
