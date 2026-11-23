// diag_services.go — R715 (2026-11-24): версии ВСЕХ сервисов стенда в
// диагностической сводке (дефект D3 часть 2 плана
// docs/plans/2026-11-24-remaining-defects.md).
//
// ЖИВОЙ ДЕФЕКТ. В summary.json была только версия балансера. По архиву нельзя
// было утверждать «доехали ли правки UI»: оператор видел balancer r708 и не
// видел, что webui остался r701 — то есть что часть исправлений физически не
// развёрнута.
//
// ЧТО ЗДЕСЬ. Секция services:
//
//	services.balancer    — что ЗАПЕЧЕНО в образ балансера (identity.json, а не env)
//	services.webui       — версия webui: env-тег контейнера или файл идентичности
//	services.cppworker[] — версии текстовых воркеров (уже были в backends[])
//	services.imageworker[] — версии image-воркеров (то же)
//	services.agent[]     — версии встроенных агентов
//	services.counts      — сколько кого видит балансер
//
// ПОЧЕМУ WEBUI БЕЗ СЕТЕВОГО ЗАПРОСА. Правило диагностики — сеть только к
// бэкендам кластера. Поэтому webui берётся из того, что уже есть в контейнере
// балансера: env-тег (compose кладёт WEBUI_TAG/WEBUI_VERSION — значения из того
// же deployments/.env, откуда собирается и сам webui) либо из файла
// идентичности (WEBUI_IDENTITY_FILE — копия /identity.json образа webui, если
// оператор её примонтировал). Если нет ни того, ни другого — честное
// source=not_configured с объяснением, а не пустой объект.
//
// ПОЧЕМУ НЕ «правильный» ответ через /identity.json по HTTP. У балансера нет
// адреса webui в конфиге (он не проксирует UI), а добавлять ещё один сетевой
// вызов в сборщик диагностики значило бы нарушить правило и зависеть от того,
// что webui вообще поднят: диагностика должна собираться и на полуразложенном
// стенде.
package api

import (
	"encoding/json"
	"os"
	"strings"

	"ollama-loadbalancer/pkg/types"
	"ollama-loadbalancer/pkg/version"
)

// webuiIdentityFileEnvR715 — путь к файлу идентичности webui (копия
// /identity.json его образа). Не LB_*, поэтому не требует проброса в compose:
// это адресная настройка оператора/скрипта.
const webuiIdentityFileEnvR715 = "WEBUI_IDENTITY_FILE"

// diagServicesR715 — секция services сводки.
func (s *Server) diagServicesR715() map[string]interface{} {
	cppWorkers := make([]map[string]interface{}, 0, 2)
	imageWorkers := make([]map[string]interface{}, 0, 2)
	agents := make([]map[string]interface{}, 0, 2)

	for _, b := range s.diagBundleBackendsR476() {
		switch b.Type {
		case types.BackendTypeLlamaCpp:
			cppWorkers = append(cppWorkers, diagWorkerServiceR715(b))
		case types.BackendTypeImage:
			imageWorkers = append(imageWorkers, diagWorkerServiceR715(b))
		}
		// Агент может быть у любого воркера: у image-воркера он встроенный
		// (AGENT_EMBEDDED), у текстового — отдельный контейнер. Показываем всех,
		// про кого пришла идентичность, даже если бэкенд выключен: вопрос «какой
		// агент крутится» не зависит от того, выбран ли узел в текущей раскладке.
		if b.HasAgent || strings.TrimSpace(b.AgentVersion) != "" || strings.TrimSpace(b.AgentCommit) != "" {
			agents = append(agents, map[string]interface{}{
				"backend_id": b.ID,
				"agent_id":   b.AgentID,
				"version":    firstNonEmptyTextR715(b.AgentVersion),
				"commit":     firstNonEmptyTextR715(b.AgentCommit),
			})
		}
	}

	return map[string]interface{}{
		"balancer":    diagBalancerServiceR715(),
		"webui":       diagWebUIServiceR715(),
		"cppworker":   cppWorkers,
		"imageworker": imageWorkers,
		"agent":       agents,
		"counts": map[string]interface{}{
			"cppworker":   len(cppWorkers),
			"imageworker": len(imageWorkers),
			"agent":       len(agents),
		},
	}
}

// diagBalancerServiceR715 — версия самого балансера.
//
// Источник — version.Get(): -ldflags (образ) приоритетнее identity.json,
// identity.json приоритетнее env. Поле source называет то, что РЕАЛЬНО
// сработало, чтобы расхождение «в .env одно, в контейнере другое» было видно
// без чтения кода pkg/version.
func diagBalancerServiceR715() map[string]interface{} {
	about := version.Get()
	out := map[string]interface{}{
		"service": firstNonEmptyTextR715(about.Service, "balancer"),
	}
	putNonEmptyR715(out, "version", about.Version)
	putNonEmptyR715(out, "tag", about.ImageTag)
	putNonEmptyR715(out, "commit", about.Commit)
	putNonEmptyR715(out, "commit_full", about.CommitFull)
	putNonEmptyR715(out, "commit_date", about.CommitDate)
	putNonEmptyR715(out, "build_date", about.BuildDate)
	putNonEmptyR715(out, "llama_commit", about.LlamaCommit)
	putNonEmptyR715(out, "rpc_proto", about.RPCProto)
	if id := version.BakedIdentityR486(); id != nil {
		out["source"] = "baked_identity"
		out["identity_path"] = id["path"]
	} else if strings.TrimSpace(about.Commit) != "" || strings.TrimSpace(about.Version) != "" {
		out["source"] = "env"
	} else {
		out["source"] = "unknown"
		out["note"] = "ни -ldflags, ни /app/identity.json, ни env не дали версии: локальная сборка без штампа"
	}
	return out
}

// webuiEnvKeysR715 — переменные, из которых берётся версия webui. Тег образа —
// самое достоверное, что есть в env; WEBUI_VERSION — его же build-arg (токен
// cache-busting в HTML). Коммит появляется, только если оператор/скрипт его
// передал.
var webuiEnvKeysR715 = []string{
	"WEBUI_TAG",
	"WEBUI_IMAGE_TAG",
	"WEBUI_VERSION",
	"WEBUI_COMMIT",
	"WEBUI_GIT_COMMIT",
	"WEBUI_COMMIT_FULL",
	"WEBUI_BUILD_DATE",
}

// diagWebUIServiceR715 — версия webui (env-тег или файл идентичности).
//
// Порядок: файл идентичности (это ФАКТ образа) → env (намерение оператора). Если
// файл есть, поля env всё равно отдаются отдельной картой env_hints: расхождение
// между ними — самостоятельный факт для разбора.
func diagWebUIServiceR715() map[string]interface{} {
	out := map[string]interface{}{"service": "webui"}

	hints := map[string]string{}
	for _, key := range webuiEnvKeysR715 {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			hints[key] = v
		}
	}

	if path := strings.TrimSpace(os.Getenv(webuiIdentityFileEnvR715)); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			var id map[string]interface{}
			if err := json.Unmarshal(data, &id); err == nil {
				out["source"] = "identity_file"
				out["identity_path"] = path
				putAnyNonEmptyR715(out, "version", id["version"])
				putAnyNonEmptyR715(out, "tag", id["image_tag"])
				putAnyNonEmptyR715(out, "commit", id["commit"])
				putAnyNonEmptyR715(out, "commit_full", id["commit_full"])
				putAnyNonEmptyR715(out, "commit_date", id["commit_date"])
				putAnyNonEmptyR715(out, "build_date", id["build_date"])
				if len(hints) > 0 {
					out["env_hints"] = hints
				}
				return out
			}
			out["identity_file_error"] = "файл " + path + " не разобран как JSON: " + err.Error()
		} else {
			out["identity_file_error"] = "файл " + path + " не прочитан: " + err.Error()
		}
	}

	if len(hints) > 0 {
		out["source"] = "env"
		if v := hints["WEBUI_TAG"]; v != "" {
			out["tag"] = v
		} else if v := hints["WEBUI_IMAGE_TAG"]; v != "" {
			out["tag"] = v
		}
		if v := firstNonEmptyTextR715(hints["WEBUI_VERSION"]); v != "" {
			out["version"] = v
		}
		if v := firstNonEmptyTextR715(hints["WEBUI_COMMIT"], hints["WEBUI_GIT_COMMIT"]); v != "" {
			out["commit"] = v
		}
		if v := hints["WEBUI_COMMIT_FULL"]; v != "" {
			out["commit_full"] = v
		}
		if v := hints["WEBUI_BUILD_DATE"]; v != "" {
			out["build_date"] = v
		}
		out["note"] = "источник — env контейнера балансера (WEBUI_TAG/WEBUI_VERSION из deployments/.env); " +
			"для достоверного ФАКТА смонтируйте копию /identity.json образа webui и задайте " + webuiIdentityFileEnvR715
		return out
	}

	out["source"] = "not_configured"
	out["note"] = "версия webui неизвестна: ни " + webuiIdentityFileEnvR715 +
		", ни WEBUI_TAG/WEBUI_VERSION не заданы в контейнере балансера. " +
		"Быстрое решение: -e WEBUI_TAG=<тег из deployments/.env>; точный ответ: " +
		"docker cp <webui>:/app/identity.json ./webui-identity.json + -e " + webuiIdentityFileEnvR715 + "=<путь>"
	return out
}

// diagWorkerServiceR715 — версия воркера из снимка кластера.
//
// Поля workerVersion/workerCommit приходят от самого воркера при регистрации
// (см. pkg/types.Backend.WorkerIdentitySource): это ФАКТ образа воркера, а не
// запись в .env балансера. BuildInfo оставлен как исторический источник —
// старые воркеры присылают только его.
func diagWorkerServiceR715(b types.Backend) map[string]interface{} {
	out := map[string]interface{}{
		"id":     b.ID,
		"name":   b.Name,
		"type":   string(b.Type),
		"host":   b.Host,
		"status": string(b.Status),
	}
	putNonEmptyR715(out, "version", b.WorkerVersion)
	putNonEmptyR715(out, "commit", b.WorkerCommit)
	putNonEmptyR715(out, "build_date", b.WorkerBuildDate)
	putNonEmptyR715(out, "rpc_proto", b.WorkerRPCProto)
	putNonEmptyR715(out, "build_info", b.BuildInfo)
	putNonEmptyR715(out, "identity_source", b.WorkerIdentitySource)
	if _, ok := out["version"]; !ok {
		if _, ok := out["build_info"]; !ok {
			out["note"] = "воркер не сообщил версию (старый образ или регистрация без идентичности)"
		}
	}
	return out
}

// putNonEmptyR715 — положить строковое поле, если оно не пустое.
func putNonEmptyR715(dst map[string]interface{}, key, value string) {
	if v := strings.TrimSpace(value); v != "" {
		dst[key] = v
	}
}

// putAnyNonEmptyR715 — то же для значения из JSON (любой скаляр).
func putAnyNonEmptyR715(dst map[string]interface{}, key string, value interface{}) {
	switch t := value.(type) {
	case string:
		putNonEmptyR715(dst, key, t)
	case nil:
		return
	default:
		dst[key] = t
	}
}

// firstNonEmptyTextR715 — первое непустое значение.
func firstNonEmptyTextR715(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
