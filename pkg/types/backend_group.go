// Package types — логический бэкенд (backend group), R92 (S2 плана).
//
// План: docs/plans/2026-10-07-model-distribution-multi-gpu-next-session.md
//
//	§5 правило 2: «Логический бэкенд — новый первоклассный объект: id, members[],
//	role (master/worker), strategy, vramBudget. Все существующие счётчики
//	(слоты, active, X-Backend-ID) работают с логическим id».
//
// ЧТО ЭТО. Группа физических бэкендов, между которыми разрезана ОДНА модель:
// мастер держит первую часть слоёв и открывает RPC-канал, воркеры — остальные.
// Для балансера такая группа — один бэкенд: один health, один слот в очереди,
// один X-Backend-ID. Это НЕ репликация: репликация требует, чтобы модель целиком
// влезла в каждый бэкенд (см. internal/modelreplication), а группа существует
// ровно потому, что целиком она не влезает ни в один.
//
// ЧЕГО ЗДЕСЬ НЕТ. Транспорта. S1 плана (сборка rpc-server, CPPWORKER_RPC_*)
// отвечает за то, чтобы мастер действительно умел считать на воркерах. Этот файл
// описывает только КОНФИГ и правила его валидации; рантайм-состояние группы
// (health, суммарный VRAM, слоты) считает internal/backendgroup.
package types

import (
	"fmt"
	"strings"
)

// BackendGroupStrategy — способ разрезать модель между узлами группы.
type BackendGroupStrategy string

const (
	// BackendGroupStrategyLayer — pipeline/layer split: слои распределены по
	// узлам, через сеть на каждый токен идёт активация на границе (H×bytes).
	// Основной путь плана: на 1 Гбит/с это < 0.5% времени токена (§3).
	BackendGroupStrategyLayer BackendGroupStrategy = "layer"
	// BackendGroupStrategyRow — row/tensor split: каждый слой обменивается
	// партиалами (H×bytes×L на токен). Терпимо на decode, дорого на prefill —
	// включается осознанно, с предупреждением оператору (§3, §7).
	BackendGroupStrategyRow BackendGroupStrategy = "row"
)

// backendGroupStrategies — допустимые значения (порядок = порядок в сообщениях).
var backendGroupStrategies = []BackendGroupStrategy{
	BackendGroupStrategyLayer, BackendGroupStrategyRow,
}

// BackendGroupStrategies возвращает список допустимых стратегий раскладки.
func BackendGroupStrategies() []BackendGroupStrategy {
	out := make([]BackendGroupStrategy, len(backendGroupStrategies))
	copy(out, backendGroupStrategies)
	return out
}

// ParseBackendGroupStrategy нормализует строку в BackendGroupStrategy
// (регистр и пробелы не важны; пустая строка остаётся пустой = «не задано»).
func ParseBackendGroupStrategy(s string) BackendGroupStrategy {
	return BackendGroupStrategy(strings.ToLower(strings.TrimSpace(s)))
}

// IsValidBackendGroupStrategy — допустима ли стратегия раскладки.
func IsValidBackendGroupStrategy(s string) bool {
	st := ParseBackendGroupStrategy(s)
	for _, valid := range backendGroupStrategies {
		if st == valid {
			return true
		}
	}
	return false
}

// Роли участников группы.
const (
	// BackendGroupRoleMaster — узел, который принимает HTTP-запросы клиента и
	// открывает RPC-канал к воркерам. Ровно один на группу.
	BackendGroupRoleMaster = "master"
	// BackendGroupRoleWorker — узел, который отдаёт свой GPU мастеру по RPC.
	// Как самостоятельный бэкенд не маршрутизируется.
	BackendGroupRoleWorker = "worker"
)

// BackendGroupMemberConfig — участник группы в конфиге.
//
// Порядок полей — по требованию fieldalignment (строки, затем числа).
type BackendGroupMemberConfig struct {
	// BackendID — id физического бэкенда в балансере (balancing.backends[] или
	// саморегистрация cppworker'а).
	BackendID string `json:"backendId"`
	// Role — master | worker.
	Role string `json:"role"`
	// RPCEndpoint — `host:port` RPC-сервера этого узла. Обязателен для воркера
	// (именно его мастер получает в `--rpc`); у мастера не используется.
	RPCEndpoint string `json:"rpcEndpoint,omitempty"`
	// VRAMBudgetMB — VRAM, которую этот узел отдаёт группе. 0 = взять из метрик
	// агента (фактическая свободная), это значение нужно только для
	// предварительного расчёта, когда метрик ещё нет.
	VRAMBudgetMB int `json:"vramBudgetMB,omitempty"`
}

// BackendGroupConfig — логический бэкенд в конфиге (`balancing.backendGroups[]`).
//
// Порядок полей — по требованию fieldalignment.
type BackendGroupConfig struct {
	// ID — идентификатор группы (он же X-Backend-ID для клиента).
	ID string `json:"id"`
	// Model — имя модели или маска (`*`, `?`), которую обслуживает группа.
	Model string `json:"model"`
	// Strategy — layer | row.
	Strategy string `json:"strategy"`
	// Reason — человекочитаемая причина (для логов/UI), как у placement-правил.
	Reason string `json:"reason,omitempty"`
	// Members — состав группы: ровно один master и хотя бы один worker.
	Members []BackendGroupMemberConfig `json:"members"`
}

// Master возвращает мастера группы (ok=false, если мастера нет или их больше
// одного — это ошибка валидации, и резолвер обязан её увидеть).
func (g BackendGroupConfig) Master() (BackendGroupMemberConfig, bool) {
	var master BackendGroupMemberConfig
	found := 0
	for _, m := range g.Members {
		if ParseBackendGroupRole(m.Role) == BackendGroupRoleMaster {
			master = m
			found++
		}
	}
	return master, found == 1
}

// Workers возвращает воркеров группы.
func (g BackendGroupConfig) Workers() []BackendGroupMemberConfig {
	out := make([]BackendGroupMemberConfig, 0, len(g.Members))
	for _, m := range g.Members {
		if ParseBackendGroupRole(m.Role) == BackendGroupRoleWorker {
			out = append(out, m)
		}
	}
	return out
}

// ParseBackendGroupRole нормализует роль (регистр/пробелы не важны).
func ParseBackendGroupRole(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// IsGroupWorker — участвует ли бэкенд в какой-нибудь группе как воркер.
//
// Нужно вызывающему коду, чтобы исключить воркеров из обычного выбора бэкенда:
// воркер не держит модель целиком, и запрос, ушедший на него напрямую, сломается.
func (g BackendGroupConfig) IsGroupWorker(backendID string) bool {
	for _, m := range g.Members {
		if m.BackendID == backendID && ParseBackendGroupRole(m.Role) == BackendGroupRoleWorker {
			return true
		}
	}
	return false
}

// ValidateBackendGroups проверяет `balancing.backendGroups[]` и возвращает
// список понятных ошибок (пустой список = конфиг корректен).
//
// Ошибки не блокируют запуск: они попадают в предупреждения и в
// GET /api/v1/placement, как и ошибки placement-политики (R72).
func ValidateBackendGroups(groups []BackendGroupConfig) []string {
	if len(groups) == 0 {
		return nil
	}
	var errs []string
	seenIDs := make(map[string]bool, len(groups))
	for i, g := range groups {
		prefix := fmt.Sprintf("backendGroups[%d]", i)
		id := strings.TrimSpace(g.ID)
		if id == "" {
			errs = append(errs, prefix+": пустое поле id (нужен идентификатор логического бэкенда)")
		} else if seenIDs[id] {
			errs = append(errs, fmt.Sprintf("%s: id %q повторяется — идентификатор группы должен быть уникальным", prefix, id))
		} else {
			seenIDs[id] = true
		}
		if strings.TrimSpace(g.Model) == "" {
			errs = append(errs, prefix+": пустое поле model (нужно имя модели или маска)")
		}
		if !IsValidBackendGroupStrategy(g.Strategy) {
			errs = append(errs, fmt.Sprintf("%s: неизвестная strategy %q (допустимо: layer, row)", prefix, g.Strategy))
		}

		masters := 0
		seenBackends := make(map[string]bool, len(g.Members))
		for j, m := range g.Members {
			mPrefix := fmt.Sprintf("%s.members[%d]", prefix, j)
			backendID := strings.TrimSpace(m.BackendID)
			if backendID == "" {
				errs = append(errs, mPrefix+": пустой backendId")
			} else if seenBackends[backendID] {
				errs = append(errs, fmt.Sprintf("%s: backendId %q указан дважды в одной группе", mPrefix, backendID))
			} else {
				seenBackends[backendID] = true
			}
			switch ParseBackendGroupRole(m.Role) {
			case BackendGroupRoleMaster:
				masters++
			case BackendGroupRoleWorker:
				if strings.TrimSpace(m.RPCEndpoint) == "" {
					errs = append(errs, mPrefix+": у воркера обязателен rpcEndpoint (host:port RPC-сервера) — "+
						"иначе мастер не сможет подключить его GPU")
				}
			default:
				errs = append(errs, fmt.Sprintf("%s: неизвестная роль %q (допустимо: master, worker)", mPrefix, m.Role))
			}
		}
		if masters != 1 {
			errs = append(errs, fmt.Sprintf("%s: в группе ровно один master, а найдено %d", prefix, masters))
		}
		if len(g.Members) < 2 {
			errs = append(errs, prefix+": раскладка требует минимум 2 узла (master + worker); "+
				"модель, влезающая в один бэкенд, обслуживается стратегией single")
		}
	}
	return errs
}
