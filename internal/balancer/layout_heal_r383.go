// layout_heal_r383.go — R383 (2026-11-01): АВТОЛЕЧЕНИЕ вырожденной раскладки.
//
// ЧТО ЛЕЧИМ. Воркер после загрузки сам сверяет «узлы передали / узлы стали
// устройствами модели» (R176) и отдаёт в /api/models поля layout_degraded +
// layout_degraded_reason. Живой случай (2026-11-01): узел донора держал
// соединение прошлой сессии, его единственный слот не принял нашу загрузку —
// модель легла на локальную карту, а оператор узнавал об этом по скорости.
//
// ДО R383 балансер эти поля ВООБЩЕ не читал (в types.LlamaCppModel их не было),
// то есть видел «модель загружена» и ничего больше. Лечение было ручным:
// перезапустить rpc-server узла и перезагрузить модель.
//
// ЧТО ДЕЛАЕМ. Раз в тик метрик (после обновления кэша) смотрим загруженные
// модели: если модель помечена degraded, раскладка включена и у неё есть узлы,
// на модели нет активных стримов и по этой паре ещё не исчерпаны попытки —
// лечим:
//  1. перезапускаем rpc-server тех узлов раскладки, которые обслуживаем МЫ
//     (роль donor и «поднимал сам», как в R133) — off → пауза → on;
//  2. выгружаем и загружаем модель на мастере: раскладка применяется при
//     загрузке (иначе перезапуск узла ничего не меняет).
//
// ОГРАНИЧЕНИЯ (осознанные):
//   - только для ПРОСТАИВАЮЩЕЙ модели: перезагрузка занятой модели — это обрыв
//     живого ответа, а degraded-модель и так работает медленно;
//   - кулдаун 15 минут и не более 3 попыток на пару backend|model: если узел
//     мёртв физически, лечение не должно превратиться в вечный цикл unload/load;
//   - выключение: LB_LAYOUT_HEAL=0 (по умолчанию включено — иначе автолечения
//     нет вовсе, а это и есть цель правки).
package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

const (
	// layoutHealCooldownR383 — не чаще одной попытки на пару в этот интервал.
	layoutHealCooldownR383 = 15 * time.Minute
	// layoutHealMaxAttemptsR383 — после стольких неудачных попыток оставляем
	// модель как есть и говорим об этом в лог: значит дело не в застойной
	// сессии, а в самом узле.
	layoutHealMaxAttemptsR383 = 3
	// layoutHealNodeRestartPauseR383 — пауза между «погасить» и «поднять» узел.
	layoutHealNodeRestartPauseR383 = 1200 * time.Millisecond
	// layoutHealHTTPTimeoutR383 — таймаут выгрузки/загрузки модели.
	layoutHealHTTPTimeoutR383 = 10 * time.Minute
)

// layoutHealEntryR383 — сколько раз и когда лечили пару backend|model.
type layoutHealEntryR383 struct {
	lastAt   time.Time
	attempts int
}

// layoutHealStateR383 — состояние попыток. Пакетная переменная, а не поле Proxy:
// структура Proxy собирается под fieldalignment, а состояние нужно одно на
// процесс (живёт столько же, сколько балансер).
var layoutHealStateR383 = struct {
	mu sync.Mutex
	m  map[string]*layoutHealEntryR383
}{m: map[string]*layoutHealEntryR383{}}

// layoutHealEnabledR383 — включено ли автолечение (LB_LAYOUT_HEAL, по умолчанию да).
func layoutHealEnabledR383() bool {
	// R388: реестр горячих настроек (переопределение → env → умолчание), чтобы
	// выключатель был виден в панели WebUI, а не только в .env.
	if v, ok := settings.HotInt("LB_LAYOUT_HEAL"); ok {
		return v > 0
	}
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("LB_LAYOUT_HEAL")))
	switch raw {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// layoutHealAllowedR383 — можно ли лечить пару сейчас; при разрешении сразу
// фиксирует попытку (чтобы два тика подряд не запустили два лечения).
func layoutHealAllowedR383(key string) bool {
	layoutHealStateR383.mu.Lock()
	defer layoutHealStateR383.mu.Unlock()
	e := layoutHealStateR383.m[key]
	if e == nil {
		e = &layoutHealEntryR383{}
		layoutHealStateR383.m[key] = e
	}
	if e.attempts >= layoutHealMaxAttemptsR383 {
		return false
	}
	if !e.lastAt.IsZero() && time.Since(e.lastAt) < layoutHealCooldownR383 {
		return false
	}
	e.lastAt = time.Now()
	e.attempts++
	return true
}

// resetLayoutHealR383 — сбросить счётчик (лечение удалось или оператор сменил роль).
func resetLayoutHealR383(key string) {
	layoutHealStateR383.mu.Lock()
	delete(layoutHealStateR383.m, key)
	layoutHealStateR383.mu.Unlock()
}

// layoutHealAttemptsR383 — сколько попыток уже сделано (для тестов и лога).
func layoutHealAttemptsR383(key string) int {
	layoutHealStateR383.mu.Lock()
	defer layoutHealStateR383.mu.Unlock()
	if e := layoutHealStateR383.m[key]; e != nil {
		return e.attempts
	}
	return 0
}

// shouldHealLayoutR383 — чистое правило: лечить ли эту модель.
//
// Вынесено отдельно, потому что решение «перезагружать модель» слишком дорого,
// чтобы проверять его тестом на живом прокси: здесь только входные факты.
func shouldHealLayoutR383(m types.LlamaCppModel, layoutKnown, layoutEnabled bool, servers, activeStreams int) (bool, string) {
	if !m.LayoutDegraded {
		return false, ""
	}
	if !strings.EqualFold(strings.TrimSpace(m.State), "loaded") || m.Name == "" {
		return false, "модель не завершила загрузку"
	}
	if !layoutKnown {
		return false, "воркер не сообщает состав раскладки"
	}
	if !layoutEnabled || servers == 0 {
		return false, "раскладка выключена — деградировать нечему"
	}
	if activeStreams > 0 {
		return false, "на модели есть активные стримы"
	}
	return true, ""
}

// maybeHealDegradedLayoutR383 — точка входа из поллера метрик.
func (p *Proxy) maybeHealDegradedLayoutR383(backendID string, models []types.LlamaCppModel) {
	if p == nil || !layoutHealEnabledR383() {
		return
	}
	for _, m := range models {
		if !m.LayoutDegraded {
			continue
		}
		enabled, servers, known := p.cppworkerRPCLayout(backendID)
		active := p.activeStreamsOnModel(backendID, m.Name)
		ok, why := shouldHealLayoutR383(m, known, enabled, len(servers), active)
		if !ok {
			if why != "" {
				logger.Get().Debugw("R383: автолечение раскладки не требуется/невозможно",
					"backend", backendID, "model", m.Name, "reason", why)
			}
			continue
		}
		key := backendID + "|" + m.Name
		if !layoutHealAllowedR383(key) {
			continue
		}
		go p.healDegradedLayoutR383(backendID, m.Name, m.Path, servers, key)
	}
}

// healDegradedLayoutR383 — перезапустить узлы раскладки и перезагрузить модель.
func (p *Proxy) healDegradedLayoutR383(backendID, model, path string, servers []string, key string) {
	logger.Get().Warnw("R383: вырожденная раскладка — лечим автоматически",
		"backend", backendID, "model", model,
		"attempt", layoutHealAttemptsR383(key),
		"servers", strings.Join(servers, ","))

	restarted := make([]string, 0, len(servers))
	for _, server := range servers {
		host := nodeHostOf(server)
		node := p.backendByNodeHostR383(host)
		if node == nil {
			logger.Get().Warnw("R383: узел раскладки не найден среди бэкендов — перезапустить некому",
				"server", server, "host", host)
			continue
		}
		// Перезапускаем ТОЛЬКО свой узел (роль donor): ручной тумблер оператора на
		// чужом бэкенде балансер не трогает — та же граница, что у R133.
		if !strings.EqualFold(strings.TrimSpace(node.Role), string(types.BackendRoleDonor)) {
			logger.Get().Infow("R383: узел не донор — rpc-server не перезапускаем",
				"server", server, "backend", node.ID, "role", node.Role)
			continue
		}
		port := p.rpcNodePort()
		if err := pushRPCNodeConfig(node, false, port); err != nil {
			logger.Get().Warnw("R383: не удалось погасить узел", "backend", node.ID, "error", err)
			continue
		}
		time.Sleep(layoutHealNodeRestartPauseR383)
		if err := pushRPCNodeConfig(node, true, port); err != nil {
			logger.Get().Warnw("R383: не удалось поднять узел", "backend", node.ID, "error", err)
			continue
		}
		restarted = append(restarted, node.ID)
	}

	if err := p.reloadModelForLayoutHealR383(backendID, model, path); err != nil {
		logger.Get().Errorw("R383: перезагрузка модели после лечения не удалась",
			"backend", backendID, "model", model, "restarted_nodes", restarted, "error", err)
		return
	}
	logger.Get().Infow("R383: модель перезагружена после лечения раскладки",
		"backend", backendID, "model", model, "restarted_nodes", restarted)
	// Счётчик не сбрасываем сразу: успех подтвердит следующий опрос метрик
	// (layout_degraded=false) — вот тогда и снимем пару, чтобы не копить мусор.
}

// backendByNodeHostR383 — запись бэкенда, чей хост совпадает с хостом узла.
func (p *Proxy) backendByNodeHostR383(host string) *types.Backend {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	var found *types.Backend
	for _, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(state.Backend.Host), host) &&
			state.Backend.ID != host {
			continue
		}
		// Детерминированно: при совпадении хоста берём запись с ролью donor,
		// иначе первую по id.
		if found == nil || strings.EqualFold(strings.TrimSpace(state.Backend.Role), string(types.BackendRoleDonor)) {
			found = state.Backend
		}
	}
	return found
}

// reloadModelForLayoutHealR383 — выгрузить и загрузить модель на мастере.
//
// Через HTTP самого воркера, а не через ModelManager: нам нужна ровно та же
// загрузка «без параметров», которую воркер выполняет по профилю (R364), — с
// профилем окна/квантования, а не с числами из балансера.
func (p *Proxy) reloadModelForLayoutHealR383(backendID, model, path string) error {
	b := p.backendByIDR383(backendID)
	if b == nil {
		return fmt.Errorf("backend %s не найден", backendID)
	}
	base := fmt.Sprintf("http://%s:%d", b.Host, b.CppWorkerPort)
	token := workerAPITokenForBackendR373(b)
	client := &http.Client{Timeout: layoutHealHTTPTimeoutR383}

	// 1. Выгрузка.
	unloadURL := base + "/api/models/unload?name=" + urlQueryEscapeR383(model)
	req, err := http.NewRequest(http.MethodPost, unloadURL, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("unload: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("unload: status %d", resp.StatusCode)
	}
	if path == "" {
		return fmt.Errorf("неизвестен путь модели %q — перезагрузить нечем", model)
	}

	// 2. Загрузка без параметров: окно/квантование берутся из профиля (R364).
	loadBody, err := json.Marshal(map[string]string{"name": model, "path": path})
	if err != nil {
		return err
	}
	req, err = http.NewRequest(http.MethodPost, base+"/api/models/load?wait=true&waitTimeoutSec=600", bytes.NewReader(loadBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("load: status %d", resp.StatusCode)
	}
	return nil
}

// backendByIDR383 — запись бэкенда по id.
func (p *Proxy) backendByIDR383(backendID string) *types.Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if state, ok := p.backends[backendID]; ok && state != nil {
		return state.Backend
	}
	return nil
}

// urlQueryEscapeR383 — экранирование имени модели в query (минимально и без
// лишних зависимостей на вызывающей стороне).
func urlQueryEscapeR383(s string) string {
	replacer := strings.NewReplacer(" ", "%20", "/", "%2F", ":", "%3A", "?", "%3F", "&", "%26", "#", "%23")
	return replacer.Replace(s)
}
