package api

// reload_targets_r263.go — R263 (2026-10-28): загрузка модели не должна уходить
// на мёртвый узел.
//
// ЖИВОЙ ФАКТ (стенд r262). `POST /api/v1/cluster/models/{model}/reload
// {operation:"load"}` БЕЗ явного backendId выбрал узел
// `cppworker-cyborg-bundled-agent` — offline/unhealthy, сеть до него недоступна, —
// и загрузка «ушла в никуда»: оператор нажал «загрузить» и ждал результат
// несколько минут, а по факту запрос уходил на мёртвый хост и завершался ошибкой
// позже и в другом месте. С явным backendId та же операция сработала сразу.
//
// ПРИЧИНА. `selectReloadTargets` собирал ВСЕ llama_cpp-бэкенды из cluster state
// (или ровно указанный, не глядя на его состояние). Состояние узла не
// проверялось вообще: offline, unhealthy, draining и «исключённый оператором»
// (R250 `Disabled`) попадали в цели загрузки наравне с живыми.
//
// ПРАВИЛО ТЕПЕРЬ.
//   - Без явного backendId: цель — только узлы, на которые ИМЕЕТ СМЫСЛ грузить
//     (healthy, не disabled). Мёртвые узлы не «тихо» выпадают: они попадают в
//     список `skipped` ответа с причиной, чтобы оператор видел, кого не тронули.
//   - С явным backendId: уважаем выбор оператора, но если узел недоступен —
//     отвечаем СРАЗУ понятной ошибкой (503) с причиной и адресом, а не через
//     несколько минут таймаута.
//   - Если не осталось ни одного подходящего узла — 503 со списком кандидатов и
//     причиной отказа по каждому. 404 остаётся только для «в кластере вообще нет
//     llama_cpp-бэкендов» (прежняя семантика).
//
// СЕМАНТИКА УСПЕШНЫХ ОТВЕТОВ НЕ МЕНЯЕТСЯ: как и раньше, загрузка выполняется на
// каждом выбранном узле, ответ агрегирует per-backend результаты (R262 —
// 202/StillLoading — не затронут).

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// reloadTargetRejection — узел, который НЕ взят в цели загрузки, и почему.
//
// Отдаётся клиенту (WebUI показывает рядом с результатами) и пишется в лог: без
// этого «загрузилось не на всех» выглядит как случайность.
type reloadTargetRejection struct {
	BackendID string `json:"backendId"`
	Reason    string `json:"reason"`
	Address   string `json:"address,omitempty"`
}

// reloadTargetSelection — результат выбора целей загрузки.
type reloadTargetSelection struct {
	// Targets — узлы, на которые операция действительно пойдёт.
	Targets []string
	// Skipped — узлы, которые подходили по типу, но не взяты (и почему).
	Skipped []reloadTargetRejection
	// LlamaCppTotal — сколько llama_cpp-записей было в кластере ДО фильтрации.
	// Нужен, чтобы отличить «узлов нет вовсе» (404) от «есть, но все непригодны»
	// (503 с причинами).
	LlamaCppTotal int
	// Explicit — оператор указал backendId (тогда отказ адресный, а не общий).
	Explicit bool
	// ExplicitReason — причина, по которой явно указанный узел не взят.
	ExplicitReason string
}

// reloadTargetUnusableReason — можно ли гнать операцию загрузки/выгрузки на этом
// узле; "" = можно.
//
// ПОЧЕМУ ИМЕННО ТАКИЕ СОСТОЯНИЯ. Healthy — очевидно да. Пустой статус — «старая
// запись/состояние ещё не собрано»: отказывать из-за неизвестности значило бы
// ломать загрузку на стендах, где статус появляется позже первого запроса,
// поэтому пропускаем с отдельной причиной в логе. Starting — узел поднимается,
// операцию туда отправлять бессмысленно (слот займётся, а воркер ещё не готов;
// R262 показал, как выглядит «ждём 300 секунд зря»). Offline/unhealthy/draining/
// ollama_unavailable — узел недоступен или не обслуживает.
func reloadTargetUnusableReason(b types.BackendMetrics) string {
	if b.BackendType != types.BackendTypeLlamaCpp {
		return "not a llama_cpp backend"
	}
	if b.Disabled {
		reason := "excluded by operator (\"do not use\")"
		if strings.TrimSpace(b.DisabledReason) != "" {
			reason += ": " + strings.TrimSpace(b.DisabledReason)
		}
		return reason
	}
	switch b.Status {
	case types.StatusHealthy, "":
		return ""
	case types.StatusStarting:
		return "backend is still starting"
	case types.StatusUnhealthy:
		return "backend is unhealthy"
	case types.StatusOffline:
		return "backend is offline (no contact)"
	case types.StatusDraining:
		return "backend is draining"
	case types.StatusOllamaUnavailable:
		return "backend is not serving (ollama unavailable)"
	default:
		return fmt.Sprintf("backend status %q is not usable", b.Status)
	}
}

// reloadTargetAddress — host:port для сообщений (у llama_cpp это cppworker-порт).
func reloadTargetAddress(b types.BackendMetrics) string {
	port := b.CppWorkerPort
	if port <= 0 {
		port = b.OllamaPort
	}
	if b.Host == "" {
		return ""
	}
	if port <= 0 {
		return b.Host
	}
	return fmt.Sprintf("%s:%d", b.Host, port)
}

// selectReloadTargetsChecked — выбор целей загрузки/выгрузки модели (R263).
//
// backendID == "" — все пригодные llama_cpp-узлы (дедуп по физическому эндпоинту,
// как Bug #11: иначе N запросов уходят на один cppworker и гоняются в
// tryAcquireOp). backendID != "" — ровно он, но только если пригоден.
func (s *Server) selectReloadTargetsChecked(backendID string) reloadTargetSelection {
	out := reloadTargetSelection{Explicit: strings.TrimSpace(backendID) != ""}

	cs := s.getClusterState()
	if cs == nil {
		return out
	}

	// Де-дупликация: один backendId на физический эндпоинт (Bug #11).
	deduped := dedupBackendsByEndpoint(
		cs.Backends,
		func(b types.BackendMetrics) endpointKey {
			return endpointKey{Host: b.Host, Port: b.CppWorkerPort, NodeAddr: b.NodeAddr}
		},
		func(b types.BackendMetrics) bool { return b.HasAgent },
		true,
	)

	for i := range deduped {
		if deduped[i].BackendType == types.BackendTypeLlamaCpp {
			out.LlamaCppTotal++
		}
	}

	if out.Explicit {
		for i := range cs.Backends {
			b := cs.Backends[i]
			if b.ID != backendID {
				continue
			}
			if reason := reloadTargetUnusableReason(b); reason != "" {
				out.ExplicitReason = reason
				out.Skipped = append(out.Skipped, reloadTargetRejection{
					BackendID: b.ID, Reason: reason, Address: reloadTargetAddress(b),
				})
				return out
			}
			out.Targets = append(out.Targets, b.ID)
			return out
		}
		// Записи с таким ID в кластере нет.
		out.ExplicitReason = "backend not found in cluster state"
		return out
	}

	for i := range deduped {
		b := deduped[i]
		if b.BackendType != types.BackendTypeLlamaCpp {
			continue
		}
		if reason := reloadTargetUnusableReason(b); reason != "" {
			out.Skipped = append(out.Skipped, reloadTargetRejection{
				BackendID: b.ID, Reason: reason, Address: reloadTargetAddress(b),
			})
			continue
		}
		out.Targets = append(out.Targets, b.ID)
	}
	return out
}

// selectReloadTargets — прежняя сигнатура для существующих вызывающих (bulk-
// загрузка и per-backend ветка): возвращает только пригодные цели.
//
// Оставлена тонкой обёрткой, чтобы правило R263 действовало ВО ВСЕХ путях
// загрузки, а не только в одиночном reload: bulk-загрузка тоже не должна
// рассылать операции на мёртвые узлы.
func (s *Server) selectReloadTargets(backendID string) []string {
	return s.selectReloadTargetsChecked(backendID).Targets
}
