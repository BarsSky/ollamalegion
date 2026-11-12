// worker_identity_r493.go — R493-12i (2026-11-12): ЧЬЯ идентичность едет в
// регистрации бэкенда.
//
// ЖИВОЙ ДЕФЕКТ. В bundled-стеке бэкенд регистрирует встроенный АГЕНТ, и поля
// `workerVersion`/`workerCommit` он заполнял СВОИМ `version.Get()`. На стенде это
// дало `workerCommit=9d42a329`, тогда как образ `gpu-261110-1925` несёт
// `/app/identity.json` с `commit=24b3a151`. Поле читается как «чем собран
// воркер», и расхождение выглядит как «раскатали не тот образ» — при проверке
// выкладки это стоило отдельного шага. Причина: агент и воркер могут быть
// собраны разными ldflags (агент копируется в образ из отдельной сборки), и
// «моя идентичность» ≠ «идентичность узла».
//
// ПРАВИЛО. Воркер — единственный, кто знает, чем он собран: он отдаёт это по
// HTTP (`GET /api/info` → `build_version`/`build_commit`/`build_date`/
// `rpc_proto`). Агент спрашивает его и отправляет ЭТИ значения; свою
// идентичность отправляет отдельными полями (`agentVersion`/`agentCommit`), а в
// `workerIdentitySource` пишет, откуда взяты worker-поля:
//
//	"worker-http"     — ответил сам воркер (факт);
//	"agent-fallback"  — воркер не ответил, взята идентичность агента (допущение,
//	                    для bundled-образа обычно верное, но это надо видеть).
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/version"
)

// Источники идентичности воркера (см. types.Backend.WorkerIdentitySource).
const (
	workerIdentityFromWorkerR493 = "worker-http"
	workerIdentityFromAgentR493  = "agent-fallback"
)

// workerIdentityR493 — что узел сообщает о СБОРКЕ воркера.
type workerIdentityR493 struct {
	Version   string
	Commit    string
	BuildDate string
	RPCProto  string
	// Source — откуда взято (worker-http | agent-fallback).
	Source string
}

// resolveWorkerIdentityR493 — идентичность ВОРКЕРА, а не агента.
//
// Порядок:
//  1. спросить сам воркер (`/api/info`) — это факт;
//  2. воркер не ответил → идентичность агента с ПОМЕТКОЙ agent-fallback (одна
//     строка в лог: «взяли свою, воркер не ответил»).
//
// Возвращает заполненную структуру всегда (пустые поля допустимы: старый образ
// воркера мог не отдавать build-поля — тогда честно пусто, а не выдумка).
func resolveWorkerIdentityR493(a *Agent) workerIdentityR493 {
	if a != nil {
		if ident, ok := fetchWorkerIdentityR493(a); ok {
			return ident
		}
	}
	own := version.Get()
	return workerIdentityR493{
		Version:   own.Version,
		Commit:    own.Commit,
		BuildDate: own.BuildDate,
		RPCProto:  own.RPCProto,
		Source:    workerIdentityFromAgentR493,
	}
}

// fetchWorkerIdentityR493 — спросить идентичность у воркера по HTTP.
//
// Эндпоинт `/api/info` открытый (как и `/api/v1/cppworker/config`, которым агент
// уже пользуется): это метаданные сборки, не модели и не токены.
func fetchWorkerIdentityR493(a *Agent) (workerIdentityR493, bool) {
	base := a.cppWorkerBaseURLR493()
	if base == "" {
		return workerIdentityR493{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), cppWorkerProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/info", nil)
	if err != nil {
		return workerIdentityR493{}, false
	}
	client := a.httpClient
	if client == nil {
		client = &http.Client{Timeout: cppWorkerProbeTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		logWorkerIdentityFallbackR493(a, "воркер не ответил: "+err.Error())
		return workerIdentityR493{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logWorkerIdentityFallbackR493(a, fmt.Sprintf("воркер ответил HTTP %d", resp.StatusCode))
		return workerIdentityR493{}, false
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		logWorkerIdentityFallbackR493(a, "ответ воркера не JSON: "+err.Error())
		return workerIdentityR493{}, false
	}
	ident := workerIdentityR493{
		Version:   stringFieldR493(body, "build_version"),
		Commit:    stringFieldR493(body, "build_commit"),
		BuildDate: stringFieldR493(body, "build_date"),
		RPCProto:  stringFieldR493(body, "rpc_proto"),
		Source:    workerIdentityFromWorkerR493,
	}
	// Полностью пустой ответ (старый воркер без build-полей) — это не «факт», а
	// отсутствие данных: падаем в fallback с пометкой, иначе в /api/v1/backends
	// было бы пусто без объяснения, откуда пусто.
	if ident.Version == "" && ident.Commit == "" && ident.BuildDate == "" && ident.RPCProto == "" {
		logWorkerIdentityFallbackR493(a, "в /api/info воркера нет build-полей (старый образ)")
		return workerIdentityR493{}, false
	}
	return ident, true
}

// cppWorkerBaseURLR493 — адрес воркера тем же правилом, что и остальные пробы
// агента: явный CppWorkerURL, иначе host:port.
func (a *Agent) cppWorkerBaseURLR493() string {
	if a == nil {
		return ""
	}
	if base := strings.TrimSpace(a.config.CppWorkerURL); base != "" {
		return strings.TrimRight(base, "/")
	}
	host := a.extractCppWorkerHost()
	port := a.extractCppWorkerPort()
	if host == "" || port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

// logWorkerIdentityFallbackR493 — одна строка на регистрацию: молчаливая подмена
// источника идентичности — ровно тот класс, из-за которого этот долг и появился.
func logWorkerIdentityFallbackR493(a *Agent, reason string) {
	fmt.Printf("[%s] %s\n", time.Now().Format(time.RFC3339),
		fmt.Sprintf("R493: идентичность воркера не получена (%s) — беру идентичность агента, в /api/v1/backends будет workerIdentitySource=agent-fallback", reason))
}

// stringFieldR493 — строковое поле из разобранного JSON (пусто, если нет).
func stringFieldR493(body map[string]interface{}, key string) string {
	if v, ok := body[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
