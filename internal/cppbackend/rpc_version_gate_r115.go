// rpc_version_gate_r115.go — R115: узел раскладки допускается только при
// совпадении коммита llama.cpp.
//
// ЗАЧЕМ. Протокол RPC связывает мастер и узел структурами `ggml_tensor` и
// командами бэкенда. Живой случай: мастер и узел были собраны из РАЗНЫХ коммитов
// (`gpu-rpc120` от 11 окт против `1d2869c6e`), llama.cpp на несовпадение
// отвечает `GGML_ABORT` — то есть падением мастера, а не внятным отказом.
// Причём сам `rpc-server` при этом жив и молчит, поэтому диагностика упиралась
// в ручное сравнение бинарников.
//
// ОТКУДА БЕРЁТСЯ ВЕРСИЯ УЗЛА. `rpc-server` не имеет HTTP-поверхности, но на
// машине-узле обычно поднят cppworker (он и регистрируется в балансере).
// Поэтому версия спрашивается у него: `GET /api/v1/cppworker/config` →
// `config.llamaBuild` (то же поле, что печётся в образ как CPPWORKER_LLAMA_BUILD).
//
// ПРАВИЛО. Коммит узла обязан совпасть с коммитом мастера. Не совпал или
// неизвестен → узел в раскладку НЕ идёт (`version_mismatch` /
// `version_unknown` в логе), и мастер планирует по остальным устройствам.
// Явное исключение — `CPPWORKER_RPC_ALLOW_VERSION_UNKNOWN=true`: тогда
// неизвестный коммит допускается, но несовпадающий — всё равно нет.
package cppbackend

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

const (
	// rpcNodeBuildTTL — как долго держим результат опроса узла. Версия узла
	// меняется только при обновлении его образа, то есть редко; опрашивать её на
	// каждом снимке устройств (раз в 30 с) незачем.
	rpcNodeBuildTTL = 120 * time.Second
	// rpcNodeHTTPTimeout — узла может не быть в сети: проверка не должна
	// задерживать планирование.
	rpcNodeHTTPTimeout = 1500 * time.Millisecond
	// rpcNodeHTTPPort — порт cppworker-поверхности узла по умолчанию.
	rpcNodeHTTPPort = "18092"
)

// nodeHTTPPort — порт cppworker-поверхности узла. Переменная, а не константа:
// в тестах нужен произвольный порт (httptest), а на стенде порт узла может быть
// нестандартным — тогда он задаётся CPPWORKER_RPC_NODE_HTTP_PORT.
var nodeHTTPPort = envOrPort("CPPWORKER_RPC_NODE_HTTP_PORT", rpcNodeHTTPPort)

func envOrPort(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// nodeBuildState — результат опроса одного узла (кэш).
type nodeBuildState struct {
	build string
	at    time.Time
}

var (
	nodeBuildMu    sync.Mutex
	nodeBuildCache = map[string]nodeBuildState{}
)

// nodeHTTPAddr — адрес cppworker-поверхности узла по адресу rpc-server.
func nodeHTTPAddr(endpoint string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(endpoint))
	if err != nil || host == "" {
		return ""
	}
	return net.JoinHostPort(host, nodeHTTPPort)
}

// fetchNodeBuild — коммит llama.cpp, который сообщает узел ("" = не сообщил).
func fetchNodeBuild(endpoint string, timeout time.Duration) (string, error) {
	addr := nodeHTTPAddr(endpoint)
	if addr == "" {
		return "", fmt.Errorf("некорректный адрес узла %q", endpoint)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + addr + "/api/v1/cppworker/config")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("узел %s ответил HTTP %d", addr, resp.StatusCode)
	}
	var payload struct {
		Config struct {
			LlamaBuild string `json:"llamaBuild"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	return strings.TrimSpace(payload.Config.LlamaBuild), nil
}

// rpcVersionStrict — оператор требует подтверждённую версию узла.
//
// По умолчанию неизвестная версия допускается (иначе правило отнимало бы ёмкость
// у рабочих конфигураций с «голым» rpc-server). Строгий режим — осознанный выбор:
// тогда узел без подтверждённого коммита в раскладку не идёт.
func rpcVersionStrict() bool {
	v := strings.TrimSpace(os.Getenv("CPPWORKER_RPC_REQUIRE_VERSION"))
	return v == "1" || strings.EqualFold(v, "true")
}

// nodeBuildMatches — допускается ли узел в раскладку.
//
// Возвращает (допущен, причина). Причина заполняется и при допуске: она уходит
// в лог, чтобы «узел не участвует» никогда не было молчаливым.
func (b *Backend) nodeBuildMatches(endpoint string) (bool, string) {
	expect := ""
	if b != nil {
		expect = strings.TrimSpace(b.cfg.LlamaBuild)
	}

	nodeBuildMu.Lock()
	st, ok := nodeBuildCache[endpoint]
	nodeBuildMu.Unlock()
	if !ok || time.Since(st.at) >= rpcNodeBuildTTL {
		build, err := fetchNodeBuild(endpoint, rpcNodeHTTPTimeout)
		if err != nil {
			build = ""
			if logger.Get() != nil {
				logger.Get().Warnw("узел раскладки: версию узнать не удалось",
					"endpoint", endpoint, "error", err.Error())
			}
		}
		st = nodeBuildState{build: build, at: time.Now()}
		nodeBuildMu.Lock()
		nodeBuildCache[endpoint] = st
		nodeBuildMu.Unlock()
	}

	if expect == "" {
		// Мастер сам не знает своей версии (образ собран без CPPWORKER_LLAMA_BUILD):
		// сравнивать не с чем — судить не берёмся, но и не отказываем.
		return true, "master_version_unknown"
	}
	switch {
	case st.build == "":
		// Версию узла узнать не удалось: у «голого» rpc-server нет HTTP-поверхности,
		// а cppworker узла может быть недоступен с мастера.
		//
		// ЖИВОЙ УРОК (2026-10-14): первая версия правила отказывала таким узлам
		// по умолчанию, и локальный сайдкар был исключён из раскладки с
		// version_unknown — то есть правило молча отнимало ёмкость у рабочей
		// конфигурации. Поэтому по умолчанию неизвестный узел ДОПУСКАЕТСЯ (как
		// было до R115), но каждый раз громко пишется предупреждение; строгий
		// режим включает оператор.
		if declared := strings.TrimSpace(os.Getenv("CPPWORKER_RPC_EXPECT_BUILD")); declared != "" {
			// Узлы без HTTP: оператор объявляет, какой коммит на них стоит.
			// Объявление сверяется с коммитом мастера — расхождение это отказ.
			if declared != expect {
				return false, "version_declared_mismatch:" + declared + "!=" + expect
			}
			return true, "version_declared"
		}
		if rpcVersionStrict() {
			return false, "version_unknown"
		}
		return true, "version_unknown_allowed"
	case st.build != expect:
		return false, "version_mismatch:" + st.build + "!=" + expect
	default:
		return true, "version_match"
	}
}

// admittedRPCServers — узлы, допущенные в раскладку (совпал коммит).
//
// Порядок узлов сохраняется: он же порядок устройств в tensor_split, поэтому
// отсев обязан быть стабильным, а не переупорядочивающим.
func (b *Backend) admittedRPCServers() []string {
	servers := b.activeRPCServers()
	if len(servers) == 0 {
		return nil
	}
	out := make([]string, 0, len(servers))
	for _, ep := range servers {
		ok, reason := b.nodeBuildMatches(ep)
		if !ok {
			if logger.Get() != nil {
				logger.Get().Errorw("узел раскладки исключён: версия llama.cpp не совпала",
					"endpoint", ep, "reason", reason,
					"hint", "обновите образ узла до того же коммита; несовпадение "+
						"приводит к GGML_ABORT на мастере")
			}
			continue
		}
		out = append(out, ep)
	}
	return out
}

// ResetRPCVersionCacheForTest — сброс кэша версий (только для тестов).
func ResetRPCVersionCacheForTest() {
	nodeBuildMu.Lock()
	nodeBuildCache = map[string]nodeBuildState{}
	nodeBuildMu.Unlock()
}
