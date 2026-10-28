package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// rpcLink — R118 (2026-10-14): замер пропускной способности канала до узлов
// раскладки слоёв.
//
// ПОЧЕМУ ИЗ ОКРУЖЕНИЯ. Канал нельзя измерить «на глаз» из процесса: цифру даёт
// отдельный прогон scripts/measure-network.ps1 (iperf3 на этом стенде падает —
// Cygwin-сборка). Поэтому оператор кладёт результат замера в .env, балансер отдаёт
// его в записи бэкенда, а WebUI показывает в пометках раскладки вместе с датой и
// источником замера — чтобы число не выглядело «выдуманным».
type rpcLink struct {
	MeasuredAt string `json:"measured_at,omitempty"`
	Source     string `json:"source,omitempty"`
	Mbps       int    `json:"mbps,omitempty"`
}

// rpcLinkAuto — R118.15 (2026-10-14): АВТОзамер канала, сделанный самим балансером.
//
// ПОЧЕМУ ОТДЕЛЬНОЕ ПОЛЕ, А НЕ ПОДМЕНА rpcLink. Живой замер на стенде показал, что
// числа РАЗНЫЕ и означают разное:
//
//	хост↔хост (оператор, scripts/measure-network.ps1) : 889 Мбит/с
//	напрямую curl с хоста мастера в узел (64 МиБ)      : 748 Мбит/с
//	балансер (контейнер) → cppworker узла (8 МиБ)      : 99..143 Мбит/с
//
// Разница — в ПУТИ: замер балансера идёт через контейнерные сети Docker Desktop на
// обеих машинах (NAT/WSL2), а операторский — между хостами напрямую. Слои раскладки
// передаёт cppworker (тоже контейнер), то есть его путь ближе к автозамеру, чем к
// хост-замеру. Поэтому оба числа показываются РАЗДЕЛЬНО и с источниками, а не
// подменяют друг друга: «одно правильное число» здесь было бы обманом.
type rpcLinkAuto struct {
	MeasuredAt string `json:"measured_at,omitempty"`
	Source     string `json:"source,omitempty"`
	Node       string `json:"node,omitempty"`
	Mbps       int    `json:"mbps,omitempty"`
}

// rpcLinkAutoInfo — автозамер для конкретного бэкенда (пусто, если замера нет).
func (s *Server) rpcLinkAutoInfo(backendID string) rpcLinkAuto {
	if s == nil || s.proxy == nil || backendID == "" {
		return rpcLinkAuto{}
	}
	mbps, at, ok := s.proxy.RPCLinkMeasured(backendID)
	if !ok {
		return rpcLinkAuto{}
	}
	node := ""
	if b := s.proxy.GetBackend(backendID); b != nil {
		node = fmt.Sprintf("%s:%d", b.Host, b.CppWorkerPort)
	}
	return rpcLinkAuto{
		Mbps:       mbps,
		MeasuredAt: at.UTC().Format(time.RFC3339),
		Node:       node,
		Source:     "lb-rpc-link-probe (балансер → cppworker узла, через контейнерную сеть)",
	}
}
func rpcLinkInfo() rpcLink {
	out := rpcLink{
		MeasuredAt: strings.TrimSpace(os.Getenv("LB_RPC_LINK_MEASURED_AT")),
		Source:     strings.TrimSpace(os.Getenv("LB_RPC_LINK_SOURCE")),
	}
	if out.Source == "" {
		out.Source = "scripts/measure-network.ps1"
	}
	if v := strings.TrimSpace(os.Getenv("LB_RPC_LINK_MBPS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out.Mbps = n
		}
	}
	return out
}

// restoreBackendRoles — R118: поднять сохранённые роли бэкендов с диска.
//
// Роли — операторское решение, и оно обязано переживать перезапуск балансера.
// В bundled-развёртывании config.json смонтирован read-only (штатное сохранение
// состояния молча падает), поэтому роли живут в том же persistent-override, что и
// llamaCpp: /app/data/runtime-overrides/backend-roles.json (том balancer_data).
func (s *Server) restoreBackendRoles() {
	store, ok := s.overridesStore.(BackendRolesStore)
	if !ok || store == nil {
		return
	}
	roles, exists, err := store.LoadBackendRoles()
	if err != nil {
		logger.Get().Warnw(i18n.T("api.handlers_backends.restore_roles_failed"), "error", err)
		return
	}
	if !exists || len(roles) == 0 {
		return
	}
	s.backendRolesMu.Lock()
	s.backendRoles = roles
	s.backendRolesMu.Unlock()
	logger.Get().Infow(i18n.T("api.handlers_backends.restore_roles_done"), "roles", len(roles))
	// Бэкенды к этому моменту могут быть ещё не зарегистрированы (агенты
	// регистрируются позже) — тогда роль подставится в roleForBackend.
	if donors, applied, aerr := s.applyBackendRoles(); aerr != nil {
		logger.Get().Warnw(i18n.T("api.handlers_backends.restore_layout_failed"), "error", aerr)
	} else if len(applied) > 0 {
		logger.Get().Infow(i18n.T("api.handlers_backends.restore_layout_applied"),
			"donors", donors, "applied", applied)
	}
	// Страховка: записи бэкендов пересобираются НЕСКОЛЬКИМИ путями (создание,
	// правка, регистрация и heartbeat агента). Ловить их все — хрупко, поэтому
	// балансер сам поддерживает роли в записях: раз в 15 с возвращает роль тому
	// бэкенду, у которого она потерялась. Операция идемпотентна (пишем только
	// когда роль пуста), поэтому стоит копейки.
	if s.stopCh != nil {
		go s.reapplyBackendRolesLoop()
	}
	// R118.2: сразу привести раскладку главного к фактически живым узлам — чтобы
	// рестарт балансера не оставлял cppworker с мёртвым списком доноров.
	s.proxy.EnsureRPCLayoutForAll()
}

// reapplyBackendRolesLoop — R118: вернуть сохранённые роли в записи бэкендов.
//
// Зачем цикл, а не разовая подстановка: запись бэкенда пересобирается из payload
// агента (создание, правка из WebUI, регистрация, heartbeat), и агент о роли не
// знает. Живая проверка: после перезапуска балансера роли читались из файла
// (в логе «роли бэкендов восстановлены, roles:2»), но в записях оставались
// пустыми — значит запись собрал путь, который роль не переносит. Вместо охоты
// за всеми путями балансер приводит записи в соответствие сам.
func (s *Server) reapplyBackendRolesLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.reapplyBackendRolesOnce()
		}
	}
}

// reapplyBackendRolesOnce — один проход: вернуть роли, потерянные в записях.
// Возвращает число восстановленных ролей (для тестов и логов).
func (s *Server) reapplyBackendRolesOnce() int {
	s.backendRolesMu.Lock()
	saved := make(map[string]string, len(s.backendRoles))
	for id, r := range s.backendRoles {
		saved[id] = r
	}
	s.backendRolesMu.Unlock()
	if len(saved) == 0 {
		return 0
	}
	restored := 0
	all := s.proxy.GetAllBackends()
	for i := range all {
		b := &all[i]
		want := saved[b.ID]
		if want == "" || strings.TrimSpace(b.Role) == want {
			continue
		}
		if s.proxy.SetBackendRole(b.ID, want) {
			restored++
			logger.Get().Infow(i18n.T("api.handlers_backends.reapply_role_returned"),
				"backend", b.ID, "role", want)
		}
	}
	// Если что-то вернули — раскладку главного тоже приводим в соответствие.
	if restored > 0 {
		if _, _, err := s.applyBackendRoles(); err != nil {
			logger.Get().Warnw(i18n.T("api.handlers_backends.reapply_layout_failed"), "error", err)
		}
	}
	// R118.2 (2026-10-14): КАЖДЫЙ тик (не только когда роль восстановлена)
	// проверяем доступность узлов раскладки. Узел-донор мог вернуться — тогда
	// раскладка включается обратно без действий оператора; мог пропасть — тогда
	// главный переводится на локальную загрузку ДО того, как упадёт загрузка
	// модели («Remote RPC server crashed or returned malformed response»).
	// Операция идемпотентна: конфиг cppworker'а пишется только при изменении.
	s.proxy.EnsureRPCLayoutForAll()
	return restored
}

// roleForBackend — R118: роль для записи бэкенда при её (пере)сборке.
//
// Приоритет: уже стоящая в записи роль → сохранённая на диске (операторская) →
// пусто. Нужно потому, что саморегистрация агента и heartbeat пересобирают запись
// и без этого стирали роль (живая проверка: после перезапуска балансера главный
// оставался без доноров, rpcEnabled=false).
func (s *Server) roleForBackend(backendID, existingRole string) string {
	if strings.TrimSpace(existingRole) != "" {
		return existingRole
	}
	s.backendRolesMu.Lock()
	defer s.backendRolesMu.Unlock()
	if s.backendRoles == nil {
		return ""
	}
	return s.backendRoles[backendID]
}

// persistBackendRoles — R118: сохранить текущие роли на диск (best-effort).
func (s *Server) persistBackendRoles() error {
	store, ok := s.overridesStore.(BackendRolesStore)
	if !ok || store == nil {
		return nil
	}
	roles := map[string]string{}
	all := s.proxy.GetAllBackends()
	for i := range all {
		b := &all[i]
		if r := strings.TrimSpace(b.Role); r != "" {
			roles[b.ID] = r
		}
	}
	// Держим в памяти то же, что сохранили: так роль переживёт и пересборку
	// записи, и рестарт в пределах процесса.
	s.backendRolesMu.Lock()
	s.backendRoles = roles
	s.backendRolesMu.Unlock()
	if err := store.SaveBackendRoles(roles); err != nil {
		logger.Get().Warnw(i18n.T("api.handlers_backends.persist_roles_failed"), "error", err)
		return err
	}
	return nil
}

// updateBackendRole — R118 (2026-10-14): PUT /api/v1/backends/{id}/role.
//
// ЯВНОЕ УПРАВЛЕНИЕ РАСКЛАДКОЙ ИЗ WEBUI. Роль хранит балансер (Backend.Role), а
// исполняет её главный cppworker: ему уходит rpcEnabled + rpcServers, собранный из
// бэкендов с ролью «донор». Так оператор в панели говорит, кто главный, кто донор,
// а кто самодостаточный, не правя .env и не заходя на машины.
//
// Доноры остаются в списке, даже если сейчас молчат: «может происходить настройка»
// — авто-уборка «молчащих» узлов такие записи не трогает (ghost_backend_reaper).
func (s *Server) updateBackendRole(w http.ResponseWriter, r *http.Request, backendID string) {
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	role := strings.ToLower(strings.TrimSpace(req.Role))
	switch role {
	case "", types.BackendRoleStandalone, types.BackendRoleMain, types.BackendRoleDonor:
	default:
		http.Error(w, `{"error":"unknown role (use standalone|main|donor)"}`, http.StatusBadRequest)
		return
	}
	backend := s.proxy.GetBackend(backendID)
	if backend == nil {
		http.Error(w, `{"error":"backend not found"}`, http.StatusNotFound)
		return
	}
	// Роль сохраняем методом балансера: он пишет состояние на диск, иначе роль
	// не переживёт рестарт (UpdateBackend состояние не сохраняет).
	if !s.proxy.SetBackendRole(backendID, role) {
		http.Error(w, `{"error":"failed to set role"}`, http.StatusInternalServerError)
		return
	}

	donors, applied, err := s.applyBackendRoles()
	// R118.2 (2026-10-14): смену роли применяет гард балансера — он же гасит
	// раскладку у того, кто перестал быть главным (standalone/донор), и проверяет
	// доступность узлов. Раньше «самодостаточный» гасился отдельной веткой, а
	// бывший главный, ставший донором, продолжал держать прежний список.
	// R249 (2026-10-28): применение раскладки к недоступному узлу НЕ держит ответ.
	//
	// Живой замер: POST /api/v1/backends/{id}/role для донора без сети отвечал
	// 15 секунд (PUT конфига раскладки с таймаутом) — в UI это «нажал галочку,
	// реакции нет». Роль уже изменена и сохранена; раскладку применяем в фоне,
	// результат оператор увидит в ответе следующего GET /api/v1/backends.
	layoutReason := ""
	go func(id string) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Get().Errorw("EnsureRPCLayout panic (background)",
					"backend", id, "panic", fmt.Sprint(rec))
			}
		}()
		if _, r := s.proxy.EnsureRPCLayout(id); r != "" {
			logger.Get().Warnw("EnsureRPCLayout (background) не применил раскладку",
				"backend", id, "reason", r)
		}
	}(backendID)
	// R133 (2026-10-16): роль donor — приказ ЭТОМУ бэкенду поднять свой узел
	// раскладки. Без этого роль в панели была командой в пустоту: rpc-server на
	// донорской машине приходилось поднимать руками (env или тумблер на месте).
	// Делаем сразу при смене роли, а не ждём периодического прохода.
	// R249 (2026-10-28): подъём rpc-узла у ДОНОРА не держит ответ.
	//
	// Живой дефект: при смене роли на недоступном узле обработчик синхронно
	// пробовал достучаться до его RPC-порта (и при необходимости поднять
	// контейнер), из-за чего ответ приходил через десятки секунд или не приходил
	// вовсе — в UI это выглядело как «нажал галочку, реакции нет», хотя роль уже
	// изменена и сохранена. Теперь ответ отдаём сразу, а подъём узла выполняем в
	// фоне; результат виден в следующем GET /api/v1/backends (rpcNode).
	nodeReason := ""
	nodeReport := s.proxy.RPCNodeReport(backendID)
	_ = layoutReason
	_ = nodeReport
	go func(id string) {
		defer func() {
			// Фон не должен ронять процесс: паника в горутине это SIGSEGV.
			if rec := recover(); rec != nil {
				logger.Get().Errorw("EnsureRPCNodeServing panic (background)",
					"backend", id, "panic", fmt.Sprint(rec))
			}
		}()
		if _, nreason := s.proxy.EnsureRPCNodeServing(id); nreason != "" {
			logger.Get().Warnw("EnsureRPCNodeServing (background) не применил узел",
				"backend", id, "reason", nreason)
		}
	}(backendID)
	if len(applied) == 0 && role != "" {
		applied = []string{backendID}
	}
	resp := map[string]interface{}{
		"status":  "updated",
		"backend": backendID,
		"role":    role,
		"donors":  donors,
		"applied": applied,
		// R133: состояние СОБСТВЕННОГО узла раскладки этого бэкенда после смены
		// роли (для донора — поднят ли rpc-server и доступен ли он с балансера).
		// Оператору нужно видеть результат нажатия, а не догадываться.
		"rpcNode": nodeReport,
		"rpcNodeCheck": "scheduled",
	}
	if nodeReason != "" {
		resp["rpcNodeReason"] = nodeReason
	}
	// R118: роль — операторское решение, сохраняем её в persistent-override
	// (config.json в bundled-развёртывании readonly, поэтому sidecar в томе
	// balancer_data). Без этого роль терялась после перезапуска балансера.
	if perr := s.persistBackendRoles(); perr != nil {
		resp["persist_warning"] = perr.Error()
	}
	if err != nil {
		resp["warning"] = err.Error()
	}
	logger.Get().Infow(i18n.T("api.handlers_backends.role_changed"),
		"backend", backendID, "role", role, "donors", len(donors), "applied", applied)
	writeJSONResponse(w, http.StatusOK, resp)
}

// backendRPCLayout — GET/PUT /api/v1/backends/{id}/rpc-layout (R229, 2026-10-22).
//
// Тумблер раскладки, который принадлежит ОПЕРАТОРУ: режим on/off/auto хранится в
// записи бэкенда (persistent) и побеждает роль. Зачем: гард EnsureRPCLayout
// каждые 15 с (и перед каждой загрузкой) выводил состав из ролей, поэтому
// выключенная вручную раскладка возвращалась обратно, и следующая загрузка снова
// уходила на донора. Здесь оператор нажимает «Применить» и получает свой выбор,
// пока сам не вернёт «авто».
func (s *Server) backendRPCLayout(w http.ResponseWriter, r *http.Request, backendID string) {
	if s.proxy.GetBackend(backendID) == nil {
		http.Error(w, `{"error":"backend not found"}`, http.StatusNotFound)
		return
	}
	var reason string
	mutated := false
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		var req struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		var enabled *bool
		switch strings.ToLower(strings.TrimSpace(req.Mode)) {
		case "", "auto":
			enabled = nil
		case "on", "true", "enabled":
			v := true
			enabled = &v
		case "off", "false", "disabled":
			v := false
			enabled = &v
		default:
			http.Error(w, `{"error":"unknown mode (use auto|on|off)"}`, http.StatusBadRequest)
			return
		}
		if !s.proxy.SetRPCEnabledOverride(backendID, enabled) {
			http.Error(w, `{"error":"failed to store rpc layout choice"}`, http.StatusInternalServerError)
			return
		}
		// Применяем сразу, не ожидая тика ролей: оператор нажал «Применить» и должен
		// увидеть результат в этом же ответе (или warning, если воркер не ответил).
		_, reason = s.proxy.EnsureRPCLayout(backendID)
		mutated = true
		logger.Get().Infow(i18n.T("api.handlers_backends.layout_choice_saved"),
			"backend", backendID, "mode", s.proxy.RPCEnabledOverrideMode(backendID), "reason", reason)
	} else if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	enabled, servers, known := s.proxy.ReportedRPCLayout(backendID)
	st := s.proxy.LayoutGuardStatus(backendID)
	status := "ok"
	if mutated {
		status = "updated"
	}
	resp := map[string]interface{}{
		"status":  status,
		"backend": backendID,
		"mode":    s.proxy.RPCEnabledOverrideMode(backendID),
		// reported — что РЕАЛЬНО на воркере сейчас (rpcEnabled + активные узлы);
		// known=false значит «воркер ещё не присылал метрик», а не «не применилось».
		"reported": map[string]interface{}{
			"enabled": enabled,
			"servers": servers,
			"known":   known,
		},
		"degraded": st.Degraded,
		"reason":   st.Reason,
	}
	if reason != "" {
		resp["warning"] = reason
	}
	writeJSONResponse(w, http.StatusOK, resp)
}

// applyBackendRoles — R118: привести раскладку главного бэкенда к списку доноров.
//
// Возвращает список адресов доноров (host:port rpc-server), кого применили, и
// ошибку доставки (её отдаём как warning: роль уже сохранена, оператор должен
// видеть, что cppworker не подтвердил).
func (s *Server) applyBackendRoles() (donors []string, applied []string, err error) {
	port := 50052
	if v := strings.TrimSpace(os.Getenv("LB_RPC_NODE_PORT")); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			port = n
		}
	}
	var main *types.Backend
	backends := s.proxy.GetAllBackends()
	for i := range backends {
		b := &backends[i]
		switch b.Role {
		case types.BackendRoleDonor:
			host := strings.TrimSpace(b.Host)
			if host == "" {
				continue
			}
			donors = append(donors, fmt.Sprintf("%s:%d", host, port))
		case types.BackendRoleMain:
			main = b
		}
	}
	sort.Strings(donors)
	if main == nil {
		// Главный не назначен — применять некуда; доноры просто помечены.
		return donors, nil, nil
	}
	if main.CppWorkerPort <= 0 {
		return donors, nil, fmt.Errorf("%s", i18n.T("api.handlers_backends.unknown_cppworker_port", main.ID))
	}
	// R118.2 (2026-10-14): раскладку применяет гард балансера, а не прямой push:
	// он проверяет TCP-доступность узлов и отправляет ТОЛЬКО живой состав (а если
	// живых нет — выключает раскладку и грузит локально). Иначе недоступный узел
	// ронял загрузку модели: «ggml-rpc.cpp:345 Remote RPC server crashed or
	// returned malformed response» + SIGABRT.
	degraded, reason := s.proxy.EnsureRPCLayout(main.ID)
	if degraded {
		return donors, []string{main.ID}, fmt.Errorf("%s", i18n.T("api.handlers_backends.layout_degraded", reason))
	}
	return donors, []string{main.ID}, nil
}

// ============================================================
// Backends API handlers
// ============================================================

// backendsHandler - список бэкендов
func (s *Server) backendsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listBackends(w, r)
	case http.MethodPost:
		s.addBackend(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// backendHandler - управление конкретным бэкендом (включая подпути /limits, /capacity, /models)
func (s *Server) backendHandler(w http.ResponseWriter, r *http.Request) {
	// Извлечение ID и подпути
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backends/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Backend ID required", http.StatusBadRequest)
		return
	}

	backendID := parts[0]

	// Подпуть /limits
	if len(parts) > 1 && parts[1] == "limits" {
		if r.Method == http.MethodPut {
			s.updateBackendLimits(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Подпуть /capacity
	if len(parts) > 1 && parts[1] == "capacity" {
		if r.Method == http.MethodGet {
			s.backendCapacity(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Подпуть /reconfigure
	if len(parts) > 1 && parts[1] == "reconfigure" {
		s.reconfigureHandler(w, r)
		return
	}

	// Подпуть /launch-config
	if len(parts) > 1 && parts[1] == "launch-config" {
		s.backendLaunchConfigHandler(w, r)
		return
	}

	// Подпуть /role — R118 (2026-10-14): явное управление ролью бэкенда в
	// раскладке слоёв: кто главный (мастер), кто донор (узел), кто самодостаточный.
	// Требование оператора: настраивать это из WebUI, а не правкой .env, и узел
	// оставлять в списке даже если он сейчас молчит (его могут настраивать).
	if len(parts) > 1 && parts[1] == "role" {
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			s.updateBackendRole(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Подпуть /rpc-layout — R229 (2026-10-22): тумблер раскладки «вкл/выкл/авто»
	// с кнопкой «Применить» в WebUI. Решение оператора переживает тик гарда (раньше
	// выключенная раскладка возвращалась ролью главного через ~15 с — живой дефект).
	if len(parts) > 1 && parts[1] == "rpc-layout" {
		switch r.Method {
		case http.MethodGet, http.MethodPut, http.MethodPost:
			s.backendRPCLayout(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Подпуть /enabled — R250 (2026-10-28): «не использовать бэкенд».
	// Тумблер, который исключает узел из выбора балансером, НЕ останавливая его:
	// узел остаётся в списке, в метриках и в раскладке, но перестаёт получать
	// новые запросы. Ответ приходит сразу — здесь нет ни одного вызова к узлу,
	// а именно недоступный узел чаще всего и выключают.
	if len(parts) > 1 && parts[1] == "enabled" {
		switch r.Method {
		case http.MethodGet, http.MethodPost, http.MethodPut:
			s.backendEnabled(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Подпуть /measure-link — R118.15 (2026-10-14): измерить канал до узла
	// раскладки СЕЙЧАС (кнопка «Проверить канал» в панели). Балансер качает
	// известный объём с cppworker'а узла и считает Мбит/с; результат попадает в
	// запись бэкенда (rpcLink) и живёт до следующего замера.
	if len(parts) > 1 && parts[1] == "measure-link" {
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			s.measureBackendLink(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Подпуть /evacuate — плавная эвакуация бэкенда
	if len(parts) > 1 && parts[1] == "evacuate" {
		if r.Method == http.MethodPost {
			s.evacuateBackend(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Подпуть /models — управление моделями на бэкенде (GET — список, POST — операция)
	if len(parts) > 1 && parts[1] == "models" {
		s.modelManageHandler(w, r)
		return
	}

	// Round 13 (2026-07-10): /agent/heartbeat — heartbeat прикреплённого агента
	// (раньше agent слал heartbeat на /api/v1/agents/heartbeat по agentID —
	// при attach режиме обновлялся неправильный бэкенд).
	if len(parts) > 1 && parts[1] == "agent" && len(parts) > 2 && parts[2] == "heartbeat" {
		if r.Method == http.MethodPost {
			s.agentBackendHeartbeatHandler(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Round 15 (2026-07-10): /agent/metrics — метрики прикреплённого агента.
	// Та же проблема что и с heartbeat: agent шлёт на /api/v1/agents/metrics по
	// agentID, но после dedup attach бэкенд имеет другой ID → 404 "Backend not found".
	// Новый endpoint использует backendID из path.
	if len(parts) > 1 && parts[1] == "agent" && len(parts) > 2 && parts[2] == "metrics" {
		if r.Method == http.MethodPost {
			s.agentBackendMetricsHandler(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getBackend(w, r, backendID)
	case http.MethodPut:
		s.updateBackend(w, r, backendID)
	case http.MethodDelete:
		s.deleteBackend(w, r, backendID)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// listBackends - получение списка бэкендов
func (s *Server) listBackends(w http.ResponseWriter, r *http.Request) {
	backends := make([]map[string]interface{}, 0)

	// По умолчанию скрываем нерабочие бэкенды, чтобы в WebUI не отображались
	// заглушки/недоступные ноды. Параметр includeUnhealthy=true возвращает всё.
	includeUnhealthy := r.URL.Query().Get("includeUnhealthy") == "true"
	unhealthyStatuses := map[types.BackendStatus]bool{
		types.StatusUnhealthy:         true,
		types.StatusOffline:           true,
		types.StatusDraining:          true,
		types.StatusOllamaUnavailable: true,
	}

	// Получение всех бэкендов из прокси
	allBackends := s.proxy.GetAllBackends()

	// Сортировка по ID для стабильного порядка
	sort.Slice(allBackends, func(i, j int) bool {
		return allBackends[i].ID < allBackends[j].ID
	})

	// 2026-06-30: de-dup по (host, CppWorkerPort). Без этого на bundled-стеке
	// (cppworker + agent) в /api/v1/backends видно 2-3 записи на один
	// физический контейнер (cppworker-gpu-bundled + cppworker-gpu от агента).
	// На вкладке Backends WebUI это выглядит как «один и тот же бэкенд дважды»,
	// а в selectBackend вызывает race. См. internal/api/dedup.go.
	allBackends = dedupBackendsByEndpoint(
		allBackends,
		func(b types.Backend) endpointKey {
			return endpointKey{Host: b.Host, Port: backendDedupPort(b), NodeAddr: b.NodeAddr}
		},
		func(b types.Backend) bool { return b.HasAgent },
		true, // preferAgent: бэкенд с агентом даёт реальные GPU/VRAM метрики
	)

	// Получение метрик из состояния кластера
	state := s.proxy.GetClusterState()

	// Создание карты метрик для быстрого доступа
	metricsMap := make(map[string]*types.BackendMetrics)
	for i := range state.Backends {
		metricsMap[state.Backends[i].ID] = &state.Backends[i]
	}

	// Формирование ответа для каждого бэкенда
	for _, backend := range allBackends {
		// Фильтруем нерабочие бэкенды, если не запрошено явное включение.
		//
		// R118.10 (2026-10-14): узел с ЯВНОЙ ролью в раскладке показываем ВСЕГДА,
		// даже offline/unhealthy. Требование оператора — «оставлять узел в списке,
		// так как может происходить настройка»: остановленный узел (или узел,
		// который настраивают) должен быть виден в панели вместе со своей ролью,
		// иначе настраивать нечего. Обычные нерабочие записи фильтруются как раньше.
		if !includeUnhealthy && unhealthyStatuses[backend.Status] && strings.TrimSpace(backend.Role) == "" {
			continue
		}
		// R71: вместимость по единому правилу (нода-саморегистрация → n_parallel,
		// иначе операторский runtime-лимит, иначе статический max). Отчёт должен
		// совпадать с тем, на что реально встаёт admission-очередь.
		// R117: вместе с числом отдаём источник (ResolveCapacity) — см. ниже.
		maxConcurrent, capacitySource := backend.ResolveCapacity()
		// Приоритет: RuntimeMaxModels > MaxModels
		maxModels := backend.MaxModels
		if backend.RuntimeMaxModels != 0 {
			maxModels = backend.RuntimeMaxModels
		}

		backendData := map[string]interface{}{
			"id":            backend.ID,
			"name":          backend.Name,
			"host":          backend.Host,
			"ollamaPort":    backend.OllamaPort,
			"agentPort":     backend.AgentPort,
			"cppWorkerPort": backend.CppWorkerPort,
			// R-Image (2026-10-02): порт image-воркера. Найдено живым E2E:
			// POST отдавал imagePort, а GET /api/v1/backends — нет, поэтому
			// потребитель, берущий порт из списка, молча падал на дефолт 18093
			// и на нестандартном порту уходил бы не туда.
			"imagePort":                    backend.ImagePort,
			"weight":                       backend.Weight,
			"maxConcurrentRequests":        maxConcurrent,
			"maxModels":                    maxModels,
			"runtimeMaxModels":             backend.RuntimeMaxModels,
			"runtimeMaxConcurrentRequests": backend.RuntimeMaxConcurrentRequests,
			// R117 (2026-10-14): ПОЧЕМУ вместимость такая. До этого отчёт давал
			// только число, и «2» одинаково выглядело как «у модели два слота», как
			// «нода заявила n_parallel=2» и как «модели нет вовсе». Живой случай:
			// count=0 на узле, а отчёт показывал 2 — разобраться без чтения кода
			// было нельзя. Источник считает types.ResolveCapacity.
			"maxConcurrentRequestsSource": capacitySource,
			"runtimeModelSlots":           backend.RuntimeModelSlots,
			// R118: роль бэкенда в раскладке слоёв (standalone|main|donor) —
			// её выставляет оператор из WebUI (PUT .../role).
			"role": backend.Role,
			// R229 (2026-10-22): тумблер раскладки оператора: auto|on|off. В панели это
			// состояние тумблера, а не догадка по роли — иначе после «выкл» он выглядел
			// бы включённым, потому что роль главного осталась прежней.
			"rpcEnabledOverride": s.proxy.RPCEnabledOverrideMode(backend.ID),
			// R250 (2026-10-28): «не использовать бэкенд». Флаг отдаётся в СПИСКЕ,
			// а не только в ответе тумблера: панель рисует состояние из списка, и
			// без этих полей выключенный узел выглядел бы рабочим. disabledSince —
			// как давно выключен; пусто = включён.
			"disabled":       backend.Disabled,
			"disabledReason": backend.DisabledReason,
			"disabledSince":  disabledSinceRFC3339(backend.DisabledAt),
			// R118 (2026-10-14): замер пропускной способности канала до узлов
			// раскладки — из окружения балансера (LB_RPC_LINK_*), потому что
			// измерить его из процесса нельзя: число даёт оператор scripts/
			// measure-network.ps1. WebUI показывает его в пометках раскладки.
			// R118.15: rpcLink — замер ОПЕРАТОРА (LB_RPC_LINK_*, как было; смысл
			// не подменяем), rpcLinkAuto — автозамер балансера до узла (см. тип
			// rpcLinkAuto: почему это ДВА разных числа, а не одно).
			"rpcLink":     rpcLinkInfo(),
			"rpcLinkAuto": s.rpcLinkAutoInfo(backend.ID),
			// R118.2 (2026-10-14): состояние раскладки у ЭТОГО бэкенда: включена
			// ли она сейчас и не работает ли в неполном составе. Оператор видит
			// причину («узлы раскладки недоступны (…): раскладка выключена,
			// загрузка идёт локально») прямо в панели, а не только в логе.
			"rpcLayout": s.proxy.LayoutGuardStatus(backend.ID),
			// R133 (2026-10-16): состояние СОБСТВЕННОГО узла раскладки бэкенда —
			// то, что раньше было не видно вообще. Донор поднимает rpc-server по
			// приказу балансера (роль donor) и отчитывается из /api/models:
			// enabled (желание), running (процесс), listening (порт), reachable
			// (проверка с балансера). Без этого роль в панели выглядела как
			// команда без ответа, а «поднят» путалось с «доступен».
			"rpcNode": s.proxy.RPCNodeReport(backend.ID),
			// R132: откуда вклад узлов в бюджет у справочных путей
			// (none|cache|probed) — «0 МБ узлов» и «узлы не измерялись» в панели
			// выглядели одинаково.
			"vramRemoteSource": s.proxy.VramRemoteSourceFor(backend.ID),
			"labels":           backend.Labels,
			"status":           backend.Status,
			"type":             backend.Type,
			"engine":           backend.Engine,
			// Round 51.2 (2026-08-20): явный API-стиль бэкенда. Если пусто — клиент
			// (WebUI) видит только effective-значение из Type. Возвращаем ОБА:
			// apiStyle (raw, оператор-установленный) + effectiveApiStyle (resolved).
			"apiStyle":          backend.ApiStyle,
			"effectiveApiStyle": backend.EffectiveAPIStyle(),
			// Round 13 (2026-07-10): expose agent attachment info для WebUI.
			"hasAgent":         backend.HasAgent,
			"agentId":          backend.AgentID,
			"lastAgentContact": backend.LastAgentContact,
			// R101 (S1 плана 2026-10-07): версия сборки llama.cpp, которую
			// сообщил узел. Оператор видит, из чего собраны узлы раскладки:
			// расхождение — причина таймаута загрузки, а не «медленная сеть».
			// Поле отдаётся всегда (пустая строка = узел версию не сообщает).
			"buildInfo": backend.BuildInfo,
		}

		// R-Image follow-up (2026-10-02): gpuIndex — ЧИСЛО, когда индекс задан
		// явно (0 — валидная ПЕРВАЯ карта), и поля НЕТ, когда индекс неизвестен
		// (nil). Разницу обязан видеть потребитель: от неё зависит ключ лока
		// сосуществования image/text (см. types.Backend.GPUIndex).
		if backend.GPUIndex != nil {
			backendData["gpuIndex"] = *backend.GPUIndex
		}

		// Добавление метрик если они доступны
		if metrics, ok := metricsMap[backend.ID]; ok {
			backendData["timestamp"] = metrics.Timestamp
			backendData["gpu"] = metrics.GPU
			backendData["system"] = metrics.System
			backendData["prediction"] = metrics.Prediction
			// Phase 2 (2026-07-06): fallback для llama_cpp бэкендов. Без NVML в
			// agent metrics.GPU пуст (MemoryTotal=0, MemoryUsed=0). Если есть
			// данные от cppworker poller (LlamaCppMetrics.GPUInfo) — используем их.
			if backend.Type == types.BackendTypeLlamaCpp && metrics.GPU.MemoryTotal == 0 {
				// R245: снапшот под RLock — поллер переписывает поля под Lock.
				lm, _ := s.proxy.GetMetricsManager().SnapshotLlamaCppMetrics(backend.ID)
				if lm != nil && lm.GPUInfo != nil {
					devMemTotal := uint64(0)
					devMemFree := uint64(0)
					devUtil := 0.0
					if len(lm.GPUInfo.Devices) > 0 {
						d := lm.GPUInfo.Devices[0]
						devMemTotal = d.MemoryTotal
						devMemFree = d.MemoryFree
						devUtil = d.Utilization
					} else {
						devMemTotal = lm.GPUInfo.TotalVRAM
						devMemFree = lm.GPUInfo.FreeVRAM
					}
					backendData["gpu"] = map[string]interface{}{
						"usagePercent": devUtil,
						"memoryTotal":  devMemTotal,
						"memoryUsed":   devMemTotal - devMemFree,
						"memoryFree":   devMemFree,
						"temperature":  0,
						"powerUsage":   0,
						"powerLimit":   0,
						"gpuClock":     0,
						"memClock":     0,
					}
				}
			}

			// === Шаг «отображение загрузки в мониторе и вкладке бэкендов» ===
			// Если бэкенд — llama_cpp, передаём loadingModels и loadedModels напрямую
			// из LlamaCppMetrics (UI использует это для отображения спиннера и
			// elapsed-времени loading, а loadedModels — для отображения текущего
			// состояния загруженных моделей в WebUI/Monitor).
			//
			// Round 51.6.1 (2026-08-20): добавлен loadedModels + loadedModelCount.
			// До этого фикса API возвращал ТОЛЬКО loadingModels, и UI никогда
			// не видел загруженные модели (хотя callback UpdateLlamaCppModelLoaded
			// обновлял lm.LoadedModels в памяти при успешной загрузке).
			// Симптом для пользователя: "гонка за отображение состояния загруженной
			// модели — информация не обновляется даже через 30s polling".
			// Реальная причина: loadedModels в response отсутствовало.
			if backend.Type == types.BackendTypeLlamaCpp {
				// R119 (пункт 4): берём КОПИЮ метрик с приписанным происхождением
				// параметров загрузки — WebUI показывает источник рядом со
				// значением (запрос / профиль / настройки по умолчанию / cppworker).
				// GetLlamaCppMetrics отдаёт живой указатель, дописывать в него нельзя.
				if lm := s.proxy.LlamaCppMetricsWithProvenance(backend.ID); lm != nil {
					loading := lm.LoadingModels
					if loading == nil {
						loading = []types.LlamaCppModel{}
					}
					backendData["loadingModels"] = loading
					backendData["loadingModelCount"] = len(loading)

					loaded := lm.LoadedModels
					if loaded == nil {
						loaded = []types.LlamaCppModel{}
					}
					backendData["loadedModels"] = loaded
					backendData["loadedModelCount"] = len(loaded)

					// Round 54.1 (2026-08-24): AutoTune report — рекомендации
					// по оптимизации загруженных моделей (KV cache, num_ctx, layers).
					// WebUI может показать badge "sub-optimal" и предложить fix.
					// R230: метрики GPU/System приходят в МБ, AnalyzeBackend ждёт
					// байты — конвертируем (иначе freeVRAM=20000 → 0.0 GB).
					var freeVRAM, freeRAM, totalVRAM uint64
					if m := metricsMap[backend.ID]; m != nil {
						totalVRAM = balancer.MBToBytes(m.GPU.MemoryTotal)
						if m.GPU.MemoryFree > 0 {
							freeVRAM = balancer.MBToBytes(m.GPU.MemoryFree)
						} else if m.GPU.MemoryTotal > 0 {
							// estimate: total - used
							freeVRAM = balancer.MBToBytes(m.GPU.MemoryTotal)
							if used := balancer.MBToBytes(m.GPU.MemoryUsed); used < freeVRAM {
								freeVRAM -= used
							}
						}
						if m.System.MemoryFree > 0 {
							freeRAM = balancer.MBToBytes(m.System.MemoryFree)
						}
					}
					autoTuneReport := balancer.AnalyzeBackend(
						s.proxy, backend.ID, string(backend.Type), loaded, freeVRAM, freeRAM, totalVRAM)
					backendData["autoTune"] = autoTuneReport
				} else {
					backendData["loadingModels"] = []types.LlamaCppModel{}
					backendData["loadingModelCount"] = 0
					backendData["loadedModels"] = []types.LlamaCppModel{}
					backendData["loadedModelCount"] = 0
				}
			}

			ollamaMetrics := metrics.Ollama

			// FreeSlots вычисляем на основе лимитов
			if ollamaMetrics.MaxConcurrentRequests > 0 {
				freeSlots := ollamaMetrics.MaxConcurrentRequests - ollamaMetrics.ActiveRequests
				if freeSlots < 0 {
					freeSlots = 0
				}
				ollamaMetrics.FreeSlots = freeSlots
			}

			backendData["ollama"] = ollamaMetrics
		} else {
			// Пустые метрики если данные недоступны
			backendData["timestamp"] = time.Time{}
			backendData["gpu"] = types.GPUMetrics{}
			backendData["system"] = types.SystemMetrics{}
			backendData["ollama"] = types.OllamaMetrics{}
		}

		// Флаг наличия активного агента
		backendData["hasAgent"] = backend.HasAgent
		backendData["lastAgentContact"] = backend.LastAgentContact

		backends = append(backends, backendData)
	}

	resp := map[string]interface{}{
		"backends": backends,
		"total":    len(backends),
	}
	// R92 (S2 плана 2026-10-07): логические бэкенды отдаются отдельным списком.
	// Без него оператор видит два физических узла и не понимает, что они
	// обслуживают ОДНУ модель как один бэкенд (слоты, health и VRAM — общие).
	// Состояние считает internal/backendgroup, см. /api/v1/placement → groups.
	if groups := s.proxy.BackendGroupStatuses(); len(groups) > 0 {
		resp["groups"] = groups
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// getBackend - получение информации о бэкенде
func (s *Server) getBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	state := s.proxy.GetClusterState()

	for _, metrics := range state.Backends {
		if metrics.ID == backendID {
			s.writeJSON(w, http.StatusOK, metrics)
			return
		}
	}

	// Если нет метрик, возвращаем конфигурацию бэкенда
	backend := s.proxy.GetBackend(backendID)
	if backend != nil {
		// R65e: НЕ отдаём секрет наружу. GetBackend возвращает живой указатель,
		// поэтому сначала делаем копию — мутировать оригинал нельзя (сломает
		// авторизацию балансера на cppworker). Раньше клиент получал
		// cppWorkerApiToken открытым текстом в теле ответа.
		safe := *backend
		safe.CppWorkerApiToken = ""
		s.writeJSON(w, http.StatusOK, &safe)
		return
	}

	http.Error(w, "Backend not found", http.StatusNotFound)
}

// addBackend - добавление бэкенда
// isSameBackendRegistration — R70: это повторная САМОРЕГИСТРАЦИЯ того же бэкенда?
//
// Признаки (оба обязательны):
//   - метка "auto-registered" (её ставит cppworker/agent при саморегистрации;
//     ручное создание бэкенда из WebUI такой метки не имеет — для него остаётся
//     409, чтобы случайный POST не перезаписал чужую запись);
//   - совпадение host и порта с уже зарегистрированным бэкендом.
//
// R-Image (2026-09-27): учитываем imagePort. У image-бэкенда (sdworker) НЕТ
// ни Ollama-, ни llama.cpp-поверхности, поэтому он физически не может заполнить
// CppWorkerPort/OllamaPort — без этой ветки повторная саморегистрация (после
// смены host/порта или рестарта) получала 409 и запись не обновлялась.
func (s *Server) isSameBackendRegistration(req backendRequest) bool {
	existing := s.proxy.GetBackend(req.ID)
	if existing == nil {
		return false
	}
	if !hasLabelFold(req.Labels, "auto-registered") {
		return false
	}
	host := req.Host
	port := req.CppWorkerPort
	if port == 0 {
		port = req.OllamaPort
	}
	if port == 0 {
		port = req.ImagePort
	}
	if host == "" || port == 0 {
		return false
	}
	sameHost := host == existing.Host
	samePort := port == existing.CppWorkerPort || port == existing.OllamaPort ||
		port == existing.ImagePort || port == existing.EffectiveImagePort()
	return sameHost && samePort
}

// isReRegistrationOfAutoBackend — R-Image Phase 8 (2026-10-03): повторная
// саморегистрация с ИЗМЕНИВШИМСЯ адресом.
//
// ЗАЧЕМ ОТДЕЛЬНО ОТ isSameBackendRegistration. Тот требует совпадения host+порта,
// поэтому пересозданный контейнер (новый host или порт — типичный случай в
// едином docker-стенде после `up -d --build`) залипал в 409 НАВСЕГДА: воркер жив
// и здоров, а в балансере остаётся старая запись с чужим адресом. Ровно это
// выглядит как «image-бэкенд не активен в докере».
//
// Условия — все обязательны, иначе POST из WebUI мог бы перезаписать чужую
// запись по совпадению ID:
//   - у СУЩЕСТВУЮЩЕЙ записи есть метка "auto-registered" (её ставит только нода,
//     ручное создание из WebUI — нет);
//   - в запросе тоже есть "auto-registered" (это саморегистрация, а не оператор);
//   - тип бэкенда в запросе непустой и совпадает с типом существующей записи.
func (s *Server) isReRegistrationOfAutoBackend(req backendRequest) bool {
	existing := s.proxy.GetBackend(req.ID)
	if existing == nil {
		return false
	}
	if !hasLabelFold(existing.Labels, "auto-registered") || !hasLabelFold(req.Labels, "auto-registered") {
		return false
	}
	want := types.BackendType(req.BackendType)
	if want == "" {
		return false
	}
	return want == existing.Type
}

// hasLabelFold — есть ли метка (без учёта регистра).
func hasLabelFold(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

// refreshRegisteredBackend — R70: обновить параметры повторно зарегистрированного
// бэкенда (идемпотентная саморегистрация cppworker'а после перезапуска).
//
// Обновляем только то, что сообщает сама нода: вместимость (maxConcurrentReqs —
// она равна реальному n_parallel), лимит моделей, тип движка, GPU-режим, вес,
// метки и токен для внутренних вызовов. Хост/порт — из той же проверки
// isSameBackendRegistration (иначе сюда бы не попали).
func (s *Server) refreshRegisteredBackend(w http.ResponseWriter, req backendRequest) {
	existing := s.proxy.GetBackend(req.ID)
	if existing == nil {
		s.writeJSON(w, http.StatusConflict, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s disappeared during update", req.ID),
		})
		return
	}
	updated := *existing
	// Phase 8: АДРЕС тоже обновляем. Для isSameBackendRegistration он совпадает по
	// определению, а для повторной регистрации авто-бэкенда с новым адресом
	// (пересозданный контейнер) это единственный способ не залипнуть в 409:
	// иначе воркер жив, а балансер держит запись с чужим host/портом и зовёт
	// несуществующий адрес.
	if req.Host != "" {
		updated.Host = req.Host
	}
	if req.Name != "" {
		updated.Name = req.Name
	}
	if req.OllamaPort > 0 {
		updated.OllamaPort = req.OllamaPort
	}
	if req.CppWorkerPort > 0 {
		updated.CppWorkerPort = req.CppWorkerPort
	}
	if req.ImagePort > 0 {
		updated.ImagePort = req.ImagePort
	}
	if req.AgentPort > 0 {
		updated.AgentPort = req.AgentPort
	}
	if req.MaxConcurrentReqs > 0 {
		updated.MaxConcurrentReqs = req.MaxConcurrentReqs
		// R70: сбрасываем runtime-override — иначе он (значение, которое агент
		// получает обратно в ответе на heartbeat) продолжит перекрывать реальную
		// вместимость ноды, и admission-очередь останется на старом пороге.
		updated.RuntimeMaxConcurrentRequests = req.MaxConcurrentReqs
		// R70: помечаем, что вместимость пришла от ноды — heartbeat агента её
		// больше не перекрывает (см. handlers_agents.go).
		updated.RuntimeCapacityFromNode = true
	}
	if req.MaxModels > 0 {
		updated.MaxModels = req.MaxModels
	}
	if req.Weight > 0 {
		updated.Weight = req.Weight
	}
	if req.BackendType != "" {
		updated.Type = types.BackendType(req.BackendType)
	}
	if req.GPUMode != "" {
		updated.GPUMode = types.PlatformMode(req.GPUMode)
	}
	if len(req.Labels) > 0 {
		updated.Labels = req.Labels
	}
	if req.CppWorkerApiToken != "" {
		updated.CppWorkerApiToken = req.CppWorkerApiToken
	}
	// R101: версия сборки — только если узел её сообщил; пустое значение не стирает
	// уже известную (сборки, которые поля не знают, не должны обнулять проверку).
	if req.BuildInfo != "" {
		updated.BuildInfo = req.BuildInfo
	}
	// GPUIndex: нода сообщила индекс (в т.ч. 0 — явная первая карта) → принимаем;
	// ключа в payload нет → сохраняем операторское значение. Сборки, которые поля
	// не знают, не должны стирать настройку, иначе лок сосуществования молча
	// вернулся бы к хостовому.
	if req.GPUIndex != nil {
		updated.GPUIndex = types.GPUIndexPtr(*req.GPUIndex)
	}
	if err := s.proxy.UpdateBackend(req.ID, updated); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}
	logger.Get().Infow("backend re-registered: parameters refreshed (R70)",
		"backend", req.ID, "max_concurrent_requests", updated.MaxConcurrentReqs,
		"max_models", updated.MaxModels, "host", updated.Host)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"updated": true,
		"backend": updated,
	})
}

// buildInfoOrKeepR101 — версия сборки из запроса, иначе уже известная.
//
// Отдельная функция, потому что путь heartbeat пересобирает Backend целиком: без
// этого правила версия, полученная при регистрации, исчезала бы на первом же
// обновлении статуса — а вместе с ней и проверка совместимости сторон RPC.
func buildInfoOrKeepR101(fromRequest, existing string) string {
	if strings.TrimSpace(fromRequest) != "" {
		return fromRequest
	}
	return existing
}

// backendRequest — тело POST /api/v1/backends (создание/саморегистрация бэкенда).
// R70: вынесено из addBackend в именованный тип, чтобы логика идемпотентной
// повторной регистрации (isSameBackendRegistration / refreshRegisteredBackend)
// работала с тем же типом.
type backendRequest struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	OllamaPort    int    `json:"ollamaPort"`
	AgentPort     int    `json:"agentPort"`
	CppWorkerPort int    `json:"cppWorkerPort"`
	// R-Image (2026-09-27): порт image-воркера для backendType=image_cpp
	// (default 18093). Fallback: CppWorkerPort → 18093 (EffectiveImagePort).
	ImagePort int `json:"imagePort"`
	// GPUIndex — индекс GPU бэкенда (nil = не задан/неизвестно; см.
	// types.Backend.GPUIndex). Влияет только на ключ лока сосуществования
	// image/text: при известном индексе у ОБЕИХ сторон лок берётся по
	// (host, gpu), иначе по хосту. УКАЗАТЕЛЬ, а не int: 0 — валидная первая
	// карта, и в int «явно GPU 0» неотличимо от «не прислано» (тогда POST от
	// cppworker'а на GPU 0 молча оставлял бы хостовый лок).
	GPUIndex          *int     `json:"gpuIndex"`
	Weight            int      `json:"weight"`
	MaxConcurrentReqs int      `json:"maxConcurrentRequests"`
	MaxModels         int      `json:"maxModels"`
	GPUMode           string   `json:"gpuMode"`
	Labels            []string `json:"labels"`
	BackendType       string   `json:"backendType"`
	BackendEngine     string   `json:"backendEngine"`
	// Round 51.2 (2026-08-20): явный API-стиль бэкенда (ollama-native/openai-compatible).
	// Если пусто — Backend.EffectiveAPIStyle() выводит из Type.
	ApiStyle string `json:"apiStyle"`
	// BuildInfo — R101 (S1 плана 2026-10-07): версия/коммит сборки llama.cpp,
	// которую узел сообщает при регистрации. Балансер сравнивает её у узлов одной
	// раскладки (протокол RPC версионно-чувствителен) — см. types.Backend.BuildInfo.
	BuildInfo string `json:"buildInfo"`
	// Round 7 (2026-07-09): cppworker пробрасывает свой API_TOKEN при
	// регистрации, чтобы balancer мог авторизоваться на /api/models/reload
	// (authMiddleware). В bundled-режиме устраняет необходимость ручной
	// настройки CppWorkerApiToken в конфиге.
	CppWorkerApiToken string `json:"cppWorkerApiToken,omitempty"`
}

func (s *Server) addBackend(w http.ResponseWriter, r *http.Request) {
	var req backendRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	// Валидация
	if req.ID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Backend ID is required",
		})
		return
	}
	if req.Host == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Backend host is required",
		})
		return
	}

	// R-MultiHost (2026-10-07): саморегистрация узла, который выдаёт себя за
	// бэкенд, уже обслуживаемый ДРУГОЙ машиной, отвергается здесь — до всех
	// веток обновления. Иначе вторая машина с тем же compose (те же
	// backendID/host/порт) молча переписывала бы чужую запись.
	if s.rejectForeignRegistration(w, r, req.ID) {
		return
	}

	// Проверка на дубликат.
	//
	// R70 (2026-09-24): саморегистрация cppworker'а (`POST /api/v1/backends` при
	// старте) должна быть ИДЕМПОТЕНТНОЙ. Раньше на существующий ID возвращался
	// 409, и после перезапуска cppworker'а с изменённой вместимостью (например
	// n_parallel=1 вместо старой константы 4) балансер продолжал держать старые
	// параметры: admission-очередь включалась на неверном пороге, а «лишние»
	// запросы блокировались внутри cppworker без позиции в очереди и keepalive.
	//
	// Обновляем ТОЛЬКО если это тот же бэкенд (совпадают host и cppWorkerPort) —
	// иначе 409, чтобы WebUI не перезаписал чужой бэкенд по ошибке.
	if s.proxy.BackendExists(req.ID) {
		if !s.isSameBackendRegistration(req) && !s.isReRegistrationOfAutoBackend(req) {
			s.writeJSON(w, http.StatusConflict, map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Backend with ID %s already exists", req.ID),
			})
			return
		}
		// R-MultiHost (2026-10-07): обновляем и узел-владелец записи, чтобы
		// устаревший адрес не «защищал» ID от законного пересозданного
		// контейнера (и наоборот — чтобы takeover чужой живой записи уже был
		// отвергнут guard'ом выше).
		s.stampNodeAddr(req.ID, peerIP(r))
		s.refreshRegisteredBackend(w, req)
		return
	}

	// Нормализация типа бэкенда.
	// Приоритет: req.BackendType > req.BackendEngine > s.config.BackendEngine > ollama
	backendType := types.BackendType(req.BackendType)
	if backendType == "" {
		// Пытаемся определить из BackendEngine в запросе (клиент мог прислать)
		if req.BackendEngine != "" {
			backendType = types.BackendEngine(req.BackendEngine).ToBackendType()
		}
		// Если клиент не прислал — берём из глобального конфига сервера
		if backendType == "" && s.config.BackendEngine != "" {
			backendType = s.config.BackendEngine.ToBackendType()
		}
		// Последний fallback — ollama (обратная совместимость)
		if backendType == "" {
			backendType = types.BackendTypeOllama
		}
	}

	// Валидация типа бэкенда
	// R-Image (2026-09-27): добавлен image_cpp — отдельный бэкенд генерации
	// изображений (свой порт ImagePort, свой каталог моделей, OpenAI/A1111 API).
	if backendType != types.BackendTypeOllama && backendType != types.BackendTypeLlamaCpp &&
		backendType != types.BackendTypeImage {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Invalid backend type: %s. Allowed: ollama, llama_cpp, image_cpp", req.BackendType),
		})
		return
	}

	// Round 51.2 (2026-08-20): валидация apiStyle на HTTP-границе.
	// Пустое значение допустимо (EffectiveAPIStyle() выведет из Type автоматически).
	// Явное невалидное значение — 400, чтобы оператор увидел ошибку сразу, а не
	// после тихого fallback'а. Внутренний helper остаётся tolerant для state.json,
	// который мог быть записан с опечаткой до R51.2.
	if req.ApiStyle != "" && !types.APIStyle(req.ApiStyle).IsValidAPIStyle() {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Invalid apiStyle: '%s'. Allowed: ollama-native, openai-compatible (or empty for auto-inference from type)", req.ApiStyle),
		})
		return
	}

	// Проверка совместимости типа бэкенда с OperatingMode
	opMode := s.config.Balancing.OperatingMode
	if !types.IsModeCompatibleWithBackendType(opMode, backendType) {
		allowedTypes := types.ModeBackendTypes[opMode]
		allowedStr := ""
		for i, t := range allowedTypes {
			if i > 0 {
				allowedStr += ", "
			}
			allowedStr += string(t)
		}
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend type '%s' is not allowed in operating mode '%s'. Allowed types: [%s]", backendType, opMode, allowedStr),
		})
		return
	}

	// Парсинг GPU mode
	gpuMode := types.ModeAuto
	switch req.GPUMode {
	case "gpu":
		gpuMode = types.ModeGPU
	case "cpu":
		gpuMode = types.ModeCPU
	}

	// Если CppWorkerPort не задан явно, используем актуальный default 18092
	// (18091 — legacy; современные llama.cpp CppWorker слушают на 18092).
	cppWorkerPort := req.CppWorkerPort
	if cppWorkerPort == 0 && backendType == types.BackendTypeLlamaCpp {
		cppWorkerPort = 18092
	}

	// R-Image: image-бэкенд слушает свой порт (default 18093). CppWorkerPort для
	// него не трогаем — EffectiveImagePort() даст ImagePort → CppWorkerPort → 18093.
	imagePort := req.ImagePort
	if imagePort == 0 && backendType == types.BackendTypeImage {
		imagePort = types.DefaultImageWorkerPort
	}

	backend := types.Backend{
		ID:                req.ID,
		Name:              req.Name,
		Host:              req.Host,
		OllamaPort:        req.OllamaPort,
		AgentPort:         req.AgentPort,
		CppWorkerPort:     cppWorkerPort,
		ImagePort:         imagePort,
		Weight:            req.Weight,
		MaxConcurrentReqs: req.MaxConcurrentReqs,
		MaxModels:         req.MaxModels,
		Labels:            req.Labels,
		Status:            types.StatusStarting,
		GPUMode:           gpuMode,
		Type:              backendType,
		CppWorkerApiToken: req.CppWorkerApiToken,
		Engine:            types.ResolveEngine(types.BackendEngine(req.BackendEngine), backendType),
		// Round 51.2 (2026-08-20): если req.ApiStyle непусто и валидно — используем как есть.
		// Иначе Backend.EffectiveAPIStyle() выведет из Type автоматически.
		ApiStyle:  types.APIStyle(req.ApiStyle),
		BuildInfo: req.BuildInfo,
		// R118 (2026-10-14): роль бэкенда в раскладке слоёв — операторское
		// решение, оно живёт в persistent-override (backend-roles.json). Именно
		// ЭТОТ путь создаёт запись для bundled-агента (POST /api/v1/backends),
		// поэтому роль надо подставлять здесь, иначе после перезапуска балансера
		// (записи пересоздаются) главный оставался без доноров — проверено живьём.
		Role: s.roleForBackend(req.ID, ""),
	}

	// GPUIndex: копируем ЗНАЧЕНИЕ из запроса, а не переиспользуем его указатель
	// (состояние бэкенда не должно зависеть от времени жизни тела запроса).
	// nil = «индекс не объявлен» → ключ лока сосуществования остаётся хостовым;
	// &0 = нода/оператор ЯВНО сказали «первая карта».
	if req.GPUIndex != nil {
		backend.GPUIndex = types.GPUIndexPtr(*req.GPUIndex)
	}

	// Добавление бэкенда в прокси
	if err := s.proxy.AddBackend(backend); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// R-MultiHost (2026-10-07): запоминаем узел, за которым закреплена запись.
	// Без этого следующая регистрация с ТОЙ ЖЕ строкой host (вторая машина с
	// тем же compose) снова считалась бы «своей»: протокол не несёт других
	// признаков, различающих узлы.
	s.stampNodeAddr(req.ID, peerIP(r))

	// Запуск health check для нового бэкенда
	go func() {
		time.Sleep(1 * time.Second)
		backend := s.proxy.GetBackend(req.ID)
		if backend != nil {
			var healthURLs []string
			switch backendType {
			case types.BackendTypeLlamaCpp:
				healthURLs = []string{fmt.Sprintf("http://%s:%d/health", backend.Host, cppWorkerPort)}
			case types.BackendTypeImage:
				// R-Image: на порту image-бэкенда может стоять либо наш воркер
				// `sdworker` (есть /health), либо голый sd-server (есть только
				// /sdcpp/v1/capabilities). Проверяем оба адреса по очереди.
				imgPort := backend.EffectiveImagePort()
				healthURLs = []string{
					fmt.Sprintf("http://%s:%d/health", backend.Host, imgPort),
					fmt.Sprintf("http://%s:%d/sdcpp/v1/capabilities", backend.Host, imgPort),
				}
			default:
				healthURLs = []string{fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)}
			}
			client := &http.Client{Timeout: 5 * time.Second}
			healthy := false
			var lastErr error
			var lastURL string
			for _, healthURL := range healthURLs {
				lastURL = healthURL
				resp, err := client.Get(healthURL)
				if err != nil {
					lastErr = err
					continue
				}
				status := resp.StatusCode
				resp.Body.Close()
				if status == http.StatusOK {
					healthy = true
					break
				}
				lastErr = fmt.Errorf("status code: %d", status)
			}
			if healthy {
				s.proxy.UpdateBackendStatus(req.ID, types.StatusHealthy)
			} else {
				if lastErr != nil {
					logger.Get().Errorw("backend health check unreachable",
						"backend", req.ID,
						"url", lastURL,
						"error", lastErr,
					)
				}
				s.proxy.UpdateBackendStatus(req.ID, types.StatusUnhealthy)
			}
		}
	}()

	s.writeJSON(w, http.StatusCreated, map[string]interface{}{
		"success": true,
		"backend": backend,
		"message": "Backend added successfully. Health check will run automatically.",
	})
}

// cloneGPUIndex — копия указателя индекса GPU (nil-safe).
//
// Нужна там, где состояние бэкенда пересобирается из снимка (heartbeat
// агента): «неизвестно» (nil) и «явно GPU 0» (&0) обязаны сохраниться как есть,
// но новая запись не должна делить указатель со снимком, который уже отдан
// наружу (иначе запись по указателю в одном месте меняла бы индекс в другом).
func cloneGPUIndex(src *int) *int {
	if src == nil {
		return nil
	}
	return types.GPUIndexPtr(*src)
}

// applyGPUIndexUpdate — merge-семантика gpuIndex для PUT /api/v1/backends/{id}.
//
// ТРИ исхода (ровно этого требует R-Image follow-up: индекс GPU 0 должен быть
// объявляемым, а индекс — сбрасываемым):
//
//	ключа в теле нет   → current (не менять): частичный PUT из WebUI не должен
//	                     стирать настройку, от которой зависит ключ лока
//	                     сосуществования image/text;
//	"gpuIndex": null   → nil (СБРОС): индекс снова «неизвестен», лок хостовый;
//	"gpuIndex": <int>  → явный индекс; 0 — валидная ПЕРВАЯ карта.
//
// Отрицательный индекс — 400: «GPU -1» смысла не имеет, а молча проглотить его
// значило бы записать в state.json значение, которое EffectiveGPUIndex() потом
// трактует как «неизвестно» (и оператор не поймёт, почему лок не сузился).
func applyGPUIndexUpdate(raw json.RawMessage, current *int) (*int, error) {
	if len(raw) == 0 {
		return current, nil // ключ отсутствует — не менять
	}
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) == "null" {
		return nil, nil // явный сброс в «неизвестно»
	}
	var idx int
	if err := json.Unmarshal(trimmed, &idx); err != nil {
		return current, fmt.Errorf("Invalid gpuIndex: expected an integer or null, got %s", string(trimmed))
	}
	if idx < 0 {
		return current, fmt.Errorf("Invalid gpuIndex: %d (must be >= 0, or null to reset)", idx)
	}
	return types.GPUIndexPtr(idx), nil
}

// updateBackend - обновление бэкенда
func (s *Server) updateBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	var req struct {
		Name          string `json:"name"`
		Host          string `json:"host"`
		OllamaPort    int    `json:"ollamaPort"`
		AgentPort     int    `json:"agentPort"`
		CppWorkerPort int    `json:"cppWorkerPort"`
		// R-Image (2026-09-27): порт image-воркера для backendType=image_cpp
		// (default 18093). Fallback: CppWorkerPort → 18093 (EffectiveImagePort).
		ImagePort int `json:"imagePort"`
		// GPUIndex — индекс GPU. Здесь json.RawMessage, а не *int: у PUT
		// ТРИ исхода, и «ключ отсутствует» обязан отличаться от «null»:
		//   - ключа нет            → индекс не менять (частичный PUT из WebUI);
		//   - `"gpuIndex": null`   → СБРОСИТЬ индекс (снова «неизвестно»,
		//                            лок сосуществования — хостовый);
		//   - `"gpuIndex": 0`      → ЯВНО GPU 0 (валидная первая карта).
		// *int не различает первые два случая: json.Unmarshal кладёт nil и в
		// отсутствующий ключ, и в null (ровно из-за этого сбросить индекс через
		// PUT раньше было нельзя). См. applyGPUIndexUpdate.
		GPUIndex          json.RawMessage `json:"gpuIndex"`
		Weight            int             `json:"weight"`
		MaxConcurrentReqs int             `json:"maxConcurrentRequests"`
		MaxModels         int             `json:"maxModels"`
		GPUMode           string          `json:"gpuMode"`
		Labels            []string        `json:"labels"`
		BackendType       string          `json:"backendType"`
		BackendEngine     string          `json:"backendEngine"`
		// Round 51.2 (2026-08-20): явный API-стиль. Семантика:
		//   - непустое значение → устанавливается
		//   - пустая строка → сохраняется существующее значение (для сброса
		//     потребуется явный "manual reset" в state.json; см. R51.3+)
		ApiStyle string `json:"apiStyle"`
		// Round 7 (2026-07-09): см. addBackend — обновление тоже должно
		// принимать CppWorkerApiToken, иначе при re-registration (PUT update)
		// после первого запуска cppworker теряет свой токен в backend state.
		CppWorkerApiToken string `json:"cppWorkerApiToken,omitempty"`
		// R101 (S1 плана 2026-10-07): версия сборки llama.cpp — приходит и при
		// регистрации, и при heartbeat; без неё в этом запросе проверка
		// совместимости сторон RPC стиралась бы на первом обновлении статуса.
		BuildInfo string `json:"buildInfo,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	// Проверка существования бэкенда
	existing := s.proxy.GetBackend(backendID)
	if existing == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s not found", backendID),
		})
		return
	}

	// Парсинг GPU mode
	gpuMode := existing.GPUMode
	switch req.GPUMode {
	case "gpu":
		gpuMode = types.ModeGPU
	case "cpu":
		gpuMode = types.ModeCPU
	case "auto":
		gpuMode = types.ModeAuto
	}

	// Нормализация типа бэкенда
	backendType := existing.Type
	if req.BackendType != "" {
		newType := types.BackendType(req.BackendType)
		if newType != existing.Type {
			// Проверка совместимости с OperatingMode
			if !types.IsModeCompatibleWithBackendType(s.config.Balancing.OperatingMode, newType) {
				allowedTypes := types.ModeBackendTypes[s.config.Balancing.OperatingMode]
				allowedStr := ""
				for i, t := range allowedTypes {
					if i > 0 {
						allowedStr += ", "
					}
					allowedStr += string(t)
				}
				s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"error":   fmt.Sprintf("Backend type '%s' is not allowed in operating mode '%s'. Allowed types: [%s]", newType, s.config.Balancing.OperatingMode, allowedStr),
				})
				return
			}
			// Запрет смены типа при активных запросах
			if existing.ActiveRequests > 0 {
				s.writeJSON(w, http.StatusConflict, map[string]interface{}{
					"success": false,
					"error":   fmt.Sprintf("Cannot change backend type from '%s' to '%s': backend has %d active requests. Wait for requests to complete or drain the backend first.", existing.Type, newType, existing.ActiveRequests),
				})
				return
			}
			logger.Get().Warnw("backend type changed via API",
				"backend", backendID,
				"oldType", existing.Type,
				"newType", newType,
			)
		}
		backendType = newType
	}

	// Применяем CppWorkerPort если передан, иначе сохраняем существующий
	cppWorkerPort := existing.CppWorkerPort
	if req.CppWorkerPort > 0 {
		cppWorkerPort = req.CppWorkerPort
	}

	// R-Image: порт image-воркера — та же merge-семантика, что у CppWorkerPort.
	imagePort := existing.ImagePort
	if req.ImagePort > 0 {
		imagePort = req.ImagePort
	}

	// GPUIndex — merge-семантика с ВОЗМОЖНОСТЬЮ СБРОСА (см. applyGPUIndexUpdate):
	// ключа нет → не менять; null → сбросить в nil («индекс неизвестен», лок
	// сосуществования снова хостовый); 0 → явная первая карта.
	gpuIndex, gpuIndexErr := applyGPUIndexUpdate(req.GPUIndex, existing.GPUIndex)
	if gpuIndexErr != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   gpuIndexErr.Error(),
		})
		return
	}

	// Round 51.2 (2026-08-20): применяем apiStyle если передан, иначе сохраняем.
	// Если оператор явно прислал невалидное значение — 400 (тот же контракт, что
	// в addBackend, для консистентности).
	if req.ApiStyle != "" && !types.APIStyle(req.ApiStyle).IsValidAPIStyle() {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Invalid apiStyle: '%s'. Allowed: ollama-native, openai-compatible (or empty to preserve existing)", req.ApiStyle),
		})
		return
	}
	apiStyle := existing.ApiStyle
	if req.ApiStyle != "" {
		apiStyle = types.APIStyle(req.ApiStyle)
	}

	// R65d (2026-09-20): fallback для cppWorkerApiToken — как у cppWorkerPort
	// и apiStyle выше.
	//
	// Найденный дефект: поле присваивалось напрямую из запроса
	// (`CppWorkerApiToken: req.CppWorkerApiToken`), а WebUI его НЕ отправляет
	// (app-modals.js формирует payload без токена — оператор не должен видеть
	// секрет). Поэтому любое редактирование бэкенда через UI стирало токен, и
	// последующие вызовы балансер→cppworker, требующие авторизации
	// (POST /api/models/reload из per-model profiles,
	// internal/api/gguf_backend_proxy.go) начинали получать 401.
	//
	// Защита от случайного стирания: пустая строка = «не менять».
	// Осознанно очистить токен можно, прислав явный маркер "-" (сервер не сможет
	// отличить "поле не прислано" от "прислано пустым" иначе).
	cppWorkerAPIToken := existing.CppWorkerApiToken
	if req.CppWorkerApiToken != "" {
		if req.CppWorkerApiToken == "-" {
			cppWorkerAPIToken = ""
		} else {
			cppWorkerAPIToken = req.CppWorkerApiToken
		}
	}

	// R65d (2026-09-20): MERGE-семантика вместо «поле-в-поле».
	//
	// Было: структура собиралась из req целиком, поэтому ЛЮБОЙ частичный PUT
	// уничтожал не переданные поля. Практический сценарий поломки — кнопка
	// «Auto-detect cppworker port» в WebUI (app-modals.js): она отправляет
	// `{cppWorkerPort: <detected>}` без остальных полей, и бэкенд получал
	// host="", name="", weight=0, labels=nil, maxConcurrentReqs=0 — то есть
	// становился нероутируемым (пустой host).
	//
	// Теперь: база — existingConfig, поверх применяются только реально
	// переданные значения. Для строк/слайсов признак «передано» — непустое
	// значение (очистить их через эту ручку нельзя, что и правильно: host и name
	// обязательны для работающего бэкенда).
	name := existing.Name
	if req.Name != "" {
		name = req.Name
	}
	host := existing.Host
	if req.Host != "" {
		host = req.Host
	}
	ollamaPort := existing.OllamaPort
	if req.OllamaPort > 0 {
		ollamaPort = req.OllamaPort
	}
	agentPort := existing.AgentPort
	if req.AgentPort > 0 {
		agentPort = req.AgentPort
	}
	// weight/maxConcurrentReqs/maxModels: 0 = «не задано» (значение 0 смысла не
	// имеет — это отсутствие лимита, которое выражается -1).
	weight := existing.Weight
	if req.Weight > 0 {
		weight = req.Weight
	}
	maxConcurrent := existing.MaxConcurrentReqs
	if req.MaxConcurrentReqs != 0 {
		maxConcurrent = req.MaxConcurrentReqs
	}
	maxModels := existing.MaxModels
	if req.MaxModels != 0 {
		maxModels = req.MaxModels
	}
	labels := existing.Labels
	if req.Labels != nil {
		labels = req.Labels
	}

	updated := types.Backend{
		ID:                backendID,
		Name:              name,
		Host:              host,
		OllamaPort:        ollamaPort,
		AgentPort:         agentPort,
		CppWorkerPort:     cppWorkerPort,
		ImagePort:         imagePort,
		GPUIndex:          gpuIndex,
		Weight:            weight,
		MaxConcurrentReqs: maxConcurrent,
		MaxModels:         maxModels,
		Labels:            labels,
		Status:            existing.Status,
		CppWorkerApiToken: cppWorkerAPIToken,
		// R101: версия сборки llama.cpp едет вместе с бэкендом — иначе heartbeat
		// (этот путь пересобирает структуру целиком) стирал бы её, и проверка
		// совместимости сторон RPC снова стала бы «неизвестно».
		BuildInfo:                    buildInfoOrKeepR101(req.BuildInfo, existing.BuildInfo),
		HasAgent:                     existing.HasAgent,
		LastAgentContact:             existing.LastAgentContact,
		LastHealthCheck:              existing.LastHealthCheck,
		ConsecutiveFailures:          existing.ConsecutiveFailures,
		ActiveRequests:               existing.ActiveRequests,
		RuntimeMaxModels:             existing.RuntimeMaxModels,
		RuntimeMaxConcurrentRequests: existing.RuntimeMaxConcurrentRequests,
		// R70: оператор меняет бэкенд из WebUI — снимаем признак «вместимость
		// от ноды», чтобы heartbeat агента снова мог её обновлять.
		RuntimeCapacityFromNode: false,
		GPUMode:                 gpuMode,
		Type:                    backendType,
		ApiStyle:                apiStyle,
		// Engine тоже не отправляется из WebUI (в request-структуре его нет).
		// Без этого он обнулялся при каждом сохранении, и бэкенд терял признак
		// движка (используется в /api/v1/backends и при выборе стратегии запуска).
		Engine: existing.Engine,
		// R-MultiHost (2026-10-07): узел-владелец — часть записи, а не то, что
		// оператор задаёт в WebUI. Реконструкция по полям его затирала, и
		// сохранение бэкенда из WebUI снимало защиту от захвата чужой машиной.
		NodeAddr: existing.NodeAddr,
		// R118 (2026-10-14): роль в раскладке слоёв — операторское решение; этот
		// путь (PUT/правка бэкенда) пересобирает запись целиком, поэтому роль
		// переносим (как Runtime*-поля и Engine выше).
		Role: s.roleForBackend(backendID, existing.Role),
		// R250 (2026-10-28): «не использовать бэкенд» — тоже операторское решение,
		// и этот путь пересобирает запись ЦЕЛИКОМ. Без переноса обычное
		// сохранение бэкенда из WebUI (например «Auto-detect cppworker port»)
		// молча возвращало выключенный узел в работу.
		Disabled:       existing.Disabled,
		DisabledReason: existing.DisabledReason,
		DisabledAt:     cloneDisabledSince(existing.DisabledAt),
	}

	// Обновление бэкенда в прокси
	if err := s.proxy.UpdateBackend(backendID, updated); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"backend": updated,
		"message": "Backend updated successfully",
	})
}

// deleteBackend - удаление бэкенда
func (s *Server) deleteBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	// Проверка существования бэкенда
	if !s.proxy.BackendExists(backendID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s not found", backendID),
		})
		return
	}

	// R118.10 (2026-10-14): УЗЕЛ С ЯВНОЙ РОЛЬЮ НЕ УДАЛЯЕМ — ОСТАВЛЯЕМ В СПИСКЕ.
	//
	// Требование оператора: «оставлять узел в списке, так как может происходить
	// настройка». Штатную остановку узла сопровождает best-effort
	// `DELETE /api/v1/backends/{id}` от самого cppworker'а
	// (cmd/cppworker/balancer_register.go) — и раньше это стирало запись вместе с
	// ролью в раскладке: остановил узел, чтобы настроить, — потерял и роль, и
	// раскладку (живая проверка: запись донора исчезала из /api/v1/backends через
	// ~30 с после docker stop).
	//
	// Теперь запись с ролью не удаляется, а «отцепляется» от агента: статус
	// offline, роль и раскладка целы, панель продолжает показывать узел и
	// позволяет его настраивать. Вернувшийся агент прикрепится к той же записи.
	// Явное удаление оператором по-прежнему возможно — сначала снимите роль
	// (PUT .../role с пустым значением), затем DELETE.
	if backend := s.proxy.GetBackend(backendID); backend != nil && strings.TrimSpace(backend.Role) != "" {
		role := strings.TrimSpace(backend.Role)
		detached := s.proxy.DetachAgentFromBackend(backendID)
		logger.Get().Warnw(i18n.T("api.handlers_backends.delete_backend_role_kept"),
			"backend", backendID, "role", role, "detached", detached,
			"hint", i18n.T("api.handlers_backends.delete_backend_role_hint"))
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success":  true,
			"id":       backendID,
			"kept":     true,
			"detached": detached,
			"role":     role,
			"message":  i18n.T("api.handlers_backends.delete_backend_role_kept_message"),
		})
		return
	}

	// Удаление бэкенда из прокси
	if err := s.proxy.RemoveBackend(backendID); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"id":      backendID,
		"message": "Backend removed successfully",
	})
}

// measureBackendLink — R118.15: POST /api/v1/backends/{id}/measure-link.
func (s *Server) measureBackendLink(w http.ResponseWriter, r *http.Request, backendID string) {
	if s.proxy == nil {
		http.Error(w, `{"error":"proxy not available"}`, http.StatusInternalServerError)
		return
	}
	mbps, err := s.proxy.MeasureRPCLinkForBackend(backendID)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"success": false,
			"backend": backendID,
			"error":   err.Error(),
		})
		return
	}
	link := s.rpcLinkAutoInfo(backendID)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"backend":  backendID,
		"mbps":     mbps,
		"measured": link.MeasuredAt,
		"node":     link.Node,
		"source":   link.Source,
	})
}

// updateBackendLimits - обновление runtime-лимитов бэкенда
func (s *Server) updateBackendLimits(w http.ResponseWriter, r *http.Request, backendID string) {
	var req struct {
		MaxModels             int `json:"maxModels"`
		MaxConcurrentRequests int `json:"maxConcurrentRequests"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	// Проверка существования бэкенда
	if !s.proxy.BackendExists(backendID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s not found", backendID),
		})
		return
	}

	// Обновление лимитов
	if err := s.proxy.UpdateBackendLimits(backendID, req.MaxModels, req.MaxConcurrentRequests); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	backend := s.proxy.GetBackend(backendID)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"backendId": backendID,
		"limits": map[string]interface{}{
			"maxModels":             req.MaxModels,
			"maxConcurrentRequests": req.MaxConcurrentRequests,
		},
		"runtime": map[string]interface{}{
			"runtimeMaxModels":             backend.RuntimeMaxModels,
			"runtimeMaxConcurrentRequests": backend.RuntimeMaxConcurrentRequests,
		},
		"message": "Backend limits updated successfully",
	})
}

// backendCapacity - детальная ёмкость конкретного бэкенда
func (s *Server) backendCapacity(w http.ResponseWriter, r *http.Request, backendID string) {
	state := s.proxy.GetClusterState()
	for _, metrics := range state.Backends {
		if metrics.ID == backendID {
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"backendId":          backendID,
				"timestamp":          time.Now().UTC(),
				"freeVram":           metrics.Ollama.BackendCapacity.FreeVRAM,
				"guaranteedVram":     metrics.Ollama.BackendCapacity.GuaranteedVRAM,
				"loadedModelVram":    metrics.Ollama.BackendCapacity.LoadedModelVRAM,
				"contextOverheadMB":  metrics.Ollama.BackendCapacity.ContextOverheadMB,
				"availableModels":    metrics.Ollama.BackendCapacity.AvailableModels,
				"loadableModelCount": metrics.Ollama.BackendCapacity.LoadableModelCount,
				"mode":               metrics.Ollama.BackendCapacity.Mode,
				"runtimeFlags":       metrics.Ollama.RuntimeFlags,
				"modelContexts":      metrics.Ollama.ModelContexts,
			})
			return
		}
	}

	http.Error(w, "Backend not found", http.StatusNotFound)
}

// modelsCapacityHandler - глобальная сводка по ёмкости моделей на всех бэкендах
func (s *Server) modelsCapacityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	type BackendCapacitySummary struct {
		BackendID          string                   `json:"backendId"`
		BackendName        string                   `json:"backendName"`
		Status             types.BackendStatus      `json:"status"`
		HasAgent           bool                     `json:"hasAgent"`
		FreeVRAM           uint64                   `json:"freeVram"`
		GuaranteedVRAM     uint64                   `json:"guaranteedVram"`
		LoadableModelCount int                      `json:"loadableModelCount"`
		Mode               types.PlatformMode       `json:"mode"`
		RuntimeFlags       types.OllamaRuntimeFlags `json:"runtimeFlags"`
		AvailableModels    []types.AvailableModel   `json:"availableModels"`
	}

	summaries := make([]BackendCapacitySummary, 0, len(state.Backends))
	totalLoadable := 0

	for _, metrics := range state.Backends {
		backend := s.proxy.GetBackend(metrics.ID)
		name := ""
		if backend != nil {
			name = backend.Name
		}

		summary := BackendCapacitySummary{
			BackendID:          metrics.ID,
			BackendName:        name,
			Status:             metrics.Status,
			HasAgent:           metrics.HasAgent,
			FreeVRAM:           metrics.Ollama.BackendCapacity.FreeVRAM,
			GuaranteedVRAM:     metrics.Ollama.BackendCapacity.GuaranteedVRAM,
			LoadableModelCount: metrics.Ollama.BackendCapacity.LoadableModelCount,
			Mode:               metrics.Ollama.BackendCapacity.Mode,
			RuntimeFlags:       metrics.Ollama.RuntimeFlags,
			AvailableModels:    metrics.Ollama.BackendCapacity.AvailableModels,
		}

		summaries = append(summaries, summary)
		totalLoadable += metrics.Ollama.BackendCapacity.LoadableModelCount
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":     time.Now().UTC(),
		"totalBackends": len(state.Backends),
		"totalLoadable": totalLoadable,
		"backends":      summaries,
	})
}

// backendLaunchConfigHandler — прокси для backendHandler subpath /launch-config
func (s *Server) backendLaunchConfigHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backends/"), "/")
	if len(parts) < 3 || parts[1] != "launch-config" {
		http.NotFound(w, r)
		return
	}
	s.listLaunchConfigs(w, r, parts[0])
}

// listLaunchConfigs — возвращает конфигурации запуска для backendId
func (s *Server) listLaunchConfigs(w http.ResponseWriter, r *http.Request, backendID string) {
	state := s.proxy.GetClusterState()
	for _, metrics := range state.Backends {
		if metrics.ID == backendID {
			gpuCount := 0
			if metrics.GPU.MemoryTotal > 0 {
				gpuCount = 1
			}
			vramTotal := metrics.GPU.MemoryTotal
			ramTotal := metrics.System.MemoryTotal

			configs := []map[string]interface{}{
				{
					"label": "Оптимальный (сбалансированный)", "type": "optimal", "isOptimal": true,
					"envVars": map[string]string{
						"OLLAMA_NUM_PARALLEL": "4", "OLLAMA_MAX_LOADED_MODELS": "2",
						"OLLAMA_KV_CACHE_TYPE": "f16", "OLLAMA_GPU_LAYERS": "-1",
						"OLLAMA_FLASH_ATTENTION": "1", "OLLAMA_NUM_THREADS": fmt.Sprintf("%d", 4),
						"OLLAMA_CONTEXT_LENGTH": "4096",
					},
					"description": "Сбалансированная конфигурация",
					"limitations": []string{},
				},
				{
					"label": "Скоростной (упор на скорость)", "type": "speed", "isOptimal": false,
					"envVars": map[string]string{
						"OLLAMA_NUM_PARALLEL": "8", "OLLAMA_MAX_LOADED_MODELS": "4",
						"OLLAMA_KV_CACHE_TYPE": "f16", "OLLAMA_GPU_LAYERS": "-1",
						"OLLAMA_FLASH_ATTENTION": "1", "OLLAMA_NUM_THREADS": fmt.Sprintf("%d", 4),
						"OLLAMA_CONTEXT_LENGTH": "4096",
					},
					"description": "Максимальный параллелизм, все модели в VRAM",
					"limitations": []string{"Высокое потребление VRAM — возможен OOM на больших моделях"},
				},
				{
					"label": "Экономный (упор на размышления)", "type": "quality", "isOptimal": false,
					"envVars": map[string]string{
						"OLLAMA_NUM_PARALLEL": "1", "OLLAMA_MAX_LOADED_MODELS": "1",
						"OLLAMA_KV_CACHE_TYPE": "q8_0", "OLLAMA_GPU_LAYERS": "-1",
						"OLLAMA_FLASH_ATTENTION": "1", "OLLAMA_NUM_THREADS": fmt.Sprintf("%d", 4/2),
						"OLLAMA_CONTEXT_LENGTH": "8192",
					},
					"description": "Один запрос с большим контекстом 8K для размышлений",
					"limitations": []string{"Однопоточный режим — другие клиенты будут ждать в очереди"},
				},
			}

			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"success":   true,
				"backendId": backendID,
				"hardware": map[string]interface{}{
					"gpuCount": gpuCount, "vramTotalMB": vramTotal,
					"ramTotalMB": ramTotal, "cpuThreads": 4,
				},
				"configs":   configs,
				"isOptimal": true,
				"message":   i18n.T("api.handlers_backends.current_config_optimal"),
			})
			return
		}
	}
	s.writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "error": "Backend not found"})
}

// reconfigureHandler — POST /api/v1/backends/{id}/reconfigure
func (s *Server) reconfigureHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backends/"), "/")
	if len(parts) < 2 || parts[1] != "reconfigure" {
		http.NotFound(w, r)
		return
	}
	backendID := parts[0]

	var req struct {
		EnvVars map[string]string `json:"envVars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	if !s.proxy.BackendExists(backendID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "Backend not found",
		})
		return
	}

	// Публикуем событие переформирования
	s.proxy.PublishEvent(types.Event{
		Type:      types.EventReconfigure,
		Timestamp: time.Now().UTC(),
		BackendID: backendID,
		Data: map[string]interface{}{
			"envVars": req.EnvVars,
		},
	})

	logger.Get().Infow("reconfigure request published", "backend", backendID, "envVars", req.EnvVars)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"backendId": backendID,
		"message":   "Reconfigure event published. Agent will apply new settings.",
	})
}

// evacuateBackend — плавная эвакуация бэкенда: переключает сессии на другие бэкенды.
// POST /api/v1/backends/{backendId}/evacuate
func (s *Server) evacuateBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	if !s.proxy.BackendExists(backendID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s not found", backendID),
		})
		return
	}

	count, err := s.proxy.EvacuateBackend(backendID)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":           true,
		"backendId":         backendID,
		"evacuatedSessions": count,
		"message":           fmt.Sprintf("Backend %s evacuated: %d sessions reassigned", backendID, count),
	})
}
