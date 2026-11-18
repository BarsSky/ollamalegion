package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
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
		// R304: пометка донора сразу в ответе на смену роли. Оператор нажал
		// «донор» — и обязан тут же узнать, что у узла осталась своя загруженная
		// копия модели (она отнимает VRAM у мастера). Иначе это выясняется только
		// в следующем списке бэкендов, а загрузка мастера к тому времени уже
		// падает по памяти.
		"donor": s.proxy.DonorMarkR304(backendID),
		// R133: состояние СОБСТВЕННОГО узла раскладки этого бэкенда после смены
		// роли (для донора — поднят ли rpc-server и доступен ли он с балансера).
		// Оператору нужно видеть результат нажатия, а не догадываться.
		"rpcNode":      nodeReport,
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
