// Package backendgroup — логический бэкенд (backend group), R92 (S2 плана
// plans/2026-10-07-model-distribution-multi-gpu-next-session.md).
//
// ЗАЧЕМ ПАКЕТ. Placement-стратегия `sharded` обещает «модель разрезана между
// бэкендами», но у балансера до R92 не было объекта, который эту раскладку
// представляет: были отдельные физические бэкенды (p.backends) и группы
// РЕПЛИКАЦИИ (internal/modelreplication), где модель целиком лежит на каждом
// узле. Логический бэкенд — третий случай: одну модель делят узлы, потому что
// целиком она не влезает ни в один.
//
// ЧТО ЗДЕСЬ ЕСТЬ:
//   - разбор `balancing.backendGroups[]` в группы (роли, RPC-эндпоинты, бюджет);
//   - состояние группы на момент запроса: жив/деградировал и ПОЧЕМУ (мастер
//     выпал, воркер не отвечает, узел не зарегистрирован);
//   - агрегированный VRAM по узлам — для fit-проверки `sharded` (S3) фит
//     считается СУММОЙ, а не максимумом: слои лежат на разных картах;
//   - слоты группы: ОДИН слот на группу (емкость мастера), а не сумма по узлам.
//     Это ровно грабли R70/R71: если посчитать слоты по всем узлам, очередь
//     решит, что вместимость удвоилась, и запустит два тяжёлых запроса в модель,
//     которой и одного не выдержать.
//
// ЧЕГО ЗДЕСЬ НЕТ: транспорта. Что мастер реально считает на воркерах — задача S1
// (сборка rpc-server, CPPWORKER_RPC_*). Пакет описывает состав и готовность.
//
// Реестр неизменяем после New: он строится из конфига и безопасен для
// конкурентного чтения из горячего пути. Свежесть данных даёт MemberInfoFunc —
// замыкание вызывающего (в балансере оно читает p.backends и метрики под
// существующими локами), поэтому пакет не зависит от internal/balancer.
package backendgroup

import (
	"sort"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// MemberInfo — всё, что реестру нужно знать о физическом узле группы.
//
// Заполняется вызывающим: индексы здоровья и VRAM живут в балансере, и тащить
// их сюда структурно значило бы завести второй источник правды.
type MemberInfo struct {
	// Status — исходный статус бэкенда (для сообщения оператору).
	Status string
	// LoadedModels — модели, ЗАГРУЖЕННЫЕ на узле прямо сейчас (R104).
	//
	// Нужны окну объединения: при «Разъединить» модели на узлах группы будут
	// выгружены, и оператор обязан увидеть, ЧТО именно потеряет, до подтверждения,
	// а не узнать об этом по таймауту следующего запроса.
	LoadedModels []string
	// VRAMFreeMB — свободная VRAM узла; осмысленна при VRAMKnown.
	VRAMFreeMB int
	// VRAMTotalMB — всего VRAM узла; осмысленна при VRAMKnown.
	VRAMTotalMB int
	// MaxConcurrentReqs — лимит одновременных запросов узла (<=0 = без лимита).
	MaxConcurrentReqs int
	// Registered — узел вообще есть в балансере.
	Registered bool
	// Healthy — узел healthy.
	Healthy bool
	// VRAMKnown — метрики VRAM доступны (иначе не гадаем).
	VRAMKnown bool
}

// MemberInfoFunc возвращает сведения об узле по его backend id.
type MemberInfoFunc func(backendID string) MemberInfo

// ModelInfo — сведения о МОДЕЛИ, которые нужны состоянию группы (R104).
//
// Скорость генерации меряется по модели, а не по узлу: у раскладки одна модель на
// всех, и «tok/s узла A» и «tok/s узла B» — это два измерения одного и того же
// процесса. Поэтому провайдер отдельный, а не поле MemberInfo.
type ModelInfo struct {
	// TokensPerSecond — измеренная скорость декода (0 = неизвестна).
	TokensPerSecond float64
	// TokensKnown — измерение есть. Отдельный флаг нужен, чтобы 0.0 не выглядел
	// как «измерили и получили ноль».
	TokensKnown bool
}

// ModelInfoFunc возвращает сведения о модели (nil = источник недоступен).
type ModelInfoFunc func(model string) ModelInfo

// Member — участник группы.
type Member struct {
	BackendID    string
	Role         string
	RPCEndpoint  string
	VRAMBudgetMB int
}

// Group — логический бэкенд: один мастер и хотя бы один воркер.
type Group struct {
	ID       string
	Model    string
	Strategy types.BackendGroupStrategy
	Reason   string
	Members  []Member
}

// Master возвращает мастера группы.
func (g *Group) Master() (Member, bool) {
	var master Member
	found := 0
	for _, m := range g.Members {
		if m.Role == types.BackendGroupRoleMaster {
			master = m
			found++
		}
	}
	return master, found == 1
}

// Workers возвращает воркеров группы.
func (g *Group) Workers() []Member {
	out := make([]Member, 0, len(g.Members))
	for _, m := range g.Members {
		if m.Role == types.BackendGroupRoleWorker {
			out = append(out, m)
		}
	}
	return out
}

// MemberState — состояние участника на момент опроса.
//
// Порядок полей — по требованию fieldalignment (строки, затем числа/bool).
type MemberState struct {
	BackendID   string `json:"backendId"`
	Role        string `json:"role"`
	RPCEndpoint string `json:"rpcEndpoint,omitempty"`
	Status      string `json:"status,omitempty"`
	Problem     string `json:"problem,omitempty"`
	// LoadedModels — что ЗАГРУЖЕНО на узле (R104): при «Разъединить» это и будет
	// выгружено, поэтому список нужен ДО подтверждения.
	LoadedModels []string `json:"loadedModels,omitempty"`
	VRAMFreeMB   int      `json:"vramFreeMB"`
	VRAMTotalMB  int      `json:"vramTotalMB"`
	Registered   bool     `json:"registered"`
	Healthy      bool     `json:"healthy"`
	VRAMKnown    bool     `json:"vramKnown"`

	// maxConcurrent — лимит слотов узла из метрик/конфига. Не экспортируется:
	// наружу слоты отдаёт только группа (Slots = слоты мастера), и публиковать
	// per-node лимит значило бы подсказывать, что их можно складывать.
	maxConcurrent int
}

// GroupState — состояние логического бэкенда (для резолвера, API и монитора).
//
// Healthy: жив ли мастер — «группа жива, пока жив мастер» (§5 плана).
// Degraded: раскладка исполнима не полностью — мастер выпал ИЛИ хотя бы один
// воркер недоступен. Degraded-группа не маршрутизируется (Usable=false): гнать
// запрос на мастера без части слоёв значит гарантированную ошибку инференса,
// а не деградацию.
//
// Порядок полей — по требованию fieldalignment.
type GroupState struct {
	ID              string        `json:"id"`
	Model           string        `json:"model"`
	Strategy        string        `json:"strategy"`
	Reason          string        `json:"reason,omitempty"`
	MasterBackendID string        `json:"masterBackendId,omitempty"`
	DegradeReason   string        `json:"degradeReason,omitempty"`
	Members         []MemberState `json:"members"`
	// LoadedModels — объединение загруженного по ВСЕМ узлам группы (R104).
	//
	// Окно объединения показывает это перед «Разъединить»: разъединение выгрузит
	// модели, и «что именно потеряется» — вопрос оператора, а не сюрприз.
	LoadedModels  []string `json:"loadedModels,omitempty"`
	VRAMFreeMB    int      `json:"vramFreeMB"`
	VRAMTotalMB   int      `json:"vramTotalMB"`
	VRAMKnownMems int      `json:"vramKnownMembers"`
	Slots         int      `json:"slots"`
	// TokensPerSecond — измеренная скорость декода модели группы (R104).
	// Источник — трекер латентности балансера; 0 без TokensKnown означает
	// «не измеряли», а не «ноль».
	TokensPerSecond float64 `json:"tokensPerSecond,omitempty"`
	Healthy         bool    `json:"healthy"`
	TokensKnown     bool    `json:"tokensKnown,omitempty"`
	Degraded        bool    `json:"degraded"`
	Usable          bool    `json:"usable"`
}

// Registry — неизменяемый набор групп + функция свежих данных об узлах.
type Registry struct {
	info      MemberInfoFunc
	modelInfo ModelInfoFunc
	groups    []*Group
	workers   map[string]string
}

// New строит реестр из конфига. info может быть nil — тогда все узлы считаются
// незарегистрированными (полезно для тестов и для отчёта «конфиг есть, узлов
// нет»).
func New(cfgs []types.BackendGroupConfig, info MemberInfoFunc) *Registry {
	return NewWithModelInfo(cfgs, info, nil)
}

// NewWithModelInfo — то же плюс источник сведений О МОДЕЛИ (R104: скорость
// декода для карточки группы). Отдельный конструктор, чтобы не менять вызовы,
// которым модель-метрики не нужны.
func NewWithModelInfo(cfgs []types.BackendGroupConfig, info MemberInfoFunc, modelInfo ModelInfoFunc) *Registry {
	r := &Registry{
		info:      info,
		modelInfo: modelInfo,
		groups:    make([]*Group, 0, len(cfgs)),
		workers:   map[string]string{},
	}
	for _, cfg := range cfgs {
		g := &Group{
			ID:       strings.TrimSpace(cfg.ID),
			Model:    strings.TrimSpace(cfg.Model),
			Strategy: types.ParseBackendGroupStrategy(cfg.Strategy),
			Reason:   cfg.Reason,
			Members:  make([]Member, 0, len(cfg.Members)),
		}
		for _, m := range cfg.Members {
			id := strings.TrimSpace(m.BackendID)
			role := types.ParseBackendGroupRole(m.Role)
			g.Members = append(g.Members, Member{
				BackendID:    id,
				Role:         role,
				RPCEndpoint:  strings.TrimSpace(m.RPCEndpoint),
				VRAMBudgetMB: m.VRAMBudgetMB,
			})
			if role == types.BackendGroupRoleWorker && id != "" {
				// Один узел — воркер ровно одной группы: иначе непонятно,
				// какие слои он отдаёт.
				r.workers[id] = g.ID
			}
		}
		r.groups = append(r.groups, g)
	}
	return r
}

// Len — сколько групп описано (в т.ч. нерабочих: их видно в состоянии).
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.groups)
}

// Configured — есть ли хоть одна группа.
func (r *Registry) Configured() bool { return r.Len() > 0 }

// Groups возвращает группы в порядке конфига.
func (r *Registry) Groups() []*Group {
	if r == nil {
		return nil
	}
	out := make([]*Group, len(r.groups))
	copy(out, r.groups)
	return out
}

// IsWorker — участвует ли узел как воркер какой-нибудь группы.
//
// Вызывающий ОБЯЗАН исключать таких узлов из обычного выбора бэкенда: воркер не
// держит модель целиком, и запрос, ушедший на него напрямую, сломается.
func (r *Registry) IsWorker(backendID string) bool {
	if r == nil {
		return false
	}
	_, ok := r.workers[backendID]
	return ok
}

// GroupIDForWorker — id группы, в которой узел является воркером ("" — не воркер).
func (r *Registry) GroupIDForWorker(backendID string) string {
	if r == nil {
		return ""
	}
	return r.workers[backendID]
}

// GroupForModel — первая группа, чья маска совпала с моделью (порядок конфига).
func (r *Registry) GroupForModel(model string) (*Group, bool) {
	if r == nil || strings.TrimSpace(model) == "" {
		return nil, false
	}
	for _, g := range r.groups {
		if types.MatchModelMask(g.Model, model) {
			return g, true
		}
	}
	return nil, false
}

// HasGroupForModel — есть ли группа под модель (без опроса состояния узлов).
func (r *Registry) HasGroupForModel(model string) bool {
	_, ok := r.GroupForModel(model)
	return ok
}

// StateForModel — состояние группы, обслуживающей модель.
func (r *Registry) StateForModel(model string) (GroupState, bool) {
	g, ok := r.GroupForModel(model)
	if !ok {
		return GroupState{}, false
	}
	return r.state(g), true
}

// StateByID — состояние группы по её id.
func (r *Registry) StateByID(groupID string) (GroupState, bool) {
	if r == nil {
		return GroupState{}, false
	}
	for _, g := range r.groups {
		if g.ID == groupID {
			return r.state(g), true
		}
	}
	return GroupState{}, false
}

// States — состояния всех групп (детерминированный порядок: по id).
func (r *Registry) States() []GroupState {
	if r == nil {
		return nil
	}
	out := make([]GroupState, 0, len(r.groups))
	for _, g := range r.groups {
		out = append(out, r.state(g))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// UsableMaster — backend id мастера, если группа ЦЕЛИКОМ исполнима.
//
// Пустая строка означает «через группу маршрутизировать нельзя»: нет группы,
// выпал мастер или недоступен хотя бы один воркер. Это и есть ответ на вопрос
// «есть ли исполнитель у стратегии sharded для этой модели СЕЙЧАС» — S3
// использует его, чтобы честно деградировать, а не отправить запрос в раскладку
// без части слоёв.
func (r *Registry) UsableMaster(model string) string {
	st, ok := r.StateForModel(model)
	if !ok || !st.Usable {
		return ""
	}
	return st.MasterBackendID
}

// state — сборка состояния группы из свежих данных об узлах.
func (r *Registry) state(g *Group) GroupState {
	st := GroupState{
		ID:       g.ID,
		Model:    g.Model,
		Strategy: string(g.Strategy),
		Reason:   g.Reason,
		Members:  make([]MemberState, 0, len(g.Members)),
		Healthy:  true,
	}
	var problems []string

	if len(g.Members) < 2 {
		st.Degraded = true
		st.Healthy = false
		problems = append(problems, "в группе меньше двух узлов — раскладка невозможна")
	}

	master, masterOK := g.Master()
	if !masterOK {
		st.Healthy = false
		st.Degraded = true
		problems = append(problems, "в группе должен быть ровно один master (ошибка конфига backendGroups)")
	}

	masterSlots := 0
	loaded := map[string]bool{}
	for _, m := range g.Members {
		ms := r.memberState(m)
		st.Members = append(st.Members, ms)
		for _, model := range ms.LoadedModels {
			loaded[model] = true
		}
		if ms.VRAMKnown {
			st.VRAMFreeMB += ms.VRAMFreeMB
			st.VRAMTotalMB += ms.VRAMTotalMB
			st.VRAMKnownMems++
		}
		if !ms.Registered {
			problems = append(problems, "узел "+m.BackendID+" ("+m.Role+") не зарегистрирован в балансере")
			if m.Role == types.BackendGroupRoleMaster {
				st.Healthy = false
			}
			st.Degraded = true
			continue
		}
		if !ms.Healthy {
			problems = append(problems, "узел "+m.BackendID+" ("+m.Role+") не healthy: "+ms.Status)
			if m.Role == types.BackendGroupRoleMaster {
				st.Healthy = false
			}
			st.Degraded = true
		}
		if m.Role == types.BackendGroupRoleMaster && masterOK && m.BackendID == master.BackendID {
			masterSlots = ms.maxConcurrent
		}
	}
	// Слоты группы = лимит МАСТЕРА, не сумма по узлам (см. шапку файла).
	st.Slots = masterSlots

	// Загруженное по группе — объединение по узлам, детерминированный порядок:
	// окно объединения показывает этот список перед «Разъединить».
	if len(loaded) > 0 {
		st.LoadedModels = make([]string, 0, len(loaded))
		for model := range loaded {
			st.LoadedModels = append(st.LoadedModels, model)
		}
		sort.Strings(st.LoadedModels)
	}

	// Скорость декода — по МОДЕЛИ группы, а не по узлам (см. ModelInfo).
	if r.modelInfo != nil && strings.TrimSpace(g.Model) != "" {
		if mi := r.modelInfo(g.Model); mi.TokensKnown {
			st.TokensPerSecond = mi.TokensPerSecond
			st.TokensKnown = true
		}
	}

	if masterOK {
		st.MasterBackendID = master.BackendID
	}
	if st.Degraded {
		st.DegradeReason = strings.Join(problems, "; ")
	}
	st.Usable = st.Healthy && !st.Degraded
	return st
}

// memberState — состояние одного участника.
func (r *Registry) memberState(m Member) MemberState {
	ms := MemberState{
		BackendID:   m.BackendID,
		Role:        m.Role,
		RPCEndpoint: m.RPCEndpoint,
	}
	if m.VRAMBudgetMB > 0 {
		// Бюджет из конфига — запасной источник, когда метрик ещё нет.
		ms.VRAMKnown = true
		ms.VRAMTotalMB = m.VRAMBudgetMB
		ms.VRAMFreeMB = m.VRAMBudgetMB
	}
	if r.info == nil {
		ms.Problem = "сведения об узлах недоступны"
		return ms
	}
	info := r.info(m.BackendID)
	ms.Registered = info.Registered
	ms.Healthy = info.Healthy
	ms.Status = info.Status
	ms.maxConcurrent = info.MaxConcurrentReqs
	if len(info.LoadedModels) > 0 {
		// Копия, а не общий слайс: состояние группы отдаётся в API, и владелец
		// метрик не должен зависеть от того, кто читает его срез.
		models := make([]string, len(info.LoadedModels))
		copy(models, info.LoadedModels)
		sort.Strings(models)
		ms.LoadedModels = models
	}
	if info.VRAMKnown {
		// Метрики точнее конфиг-бюджета: бюджет задаётся «на глаз» оператором,
		// а метрики приходят от агента с живой карты.
		ms.VRAMKnown = true
		ms.VRAMFreeMB = info.VRAMFreeMB
		ms.VRAMTotalMB = info.VRAMTotalMB
	}
	if !info.Registered {
		ms.Problem = "не зарегистрирован"
	} else if !info.Healthy {
		ms.Problem = "не healthy: " + info.Status
	} else if !ms.VRAMKnown {
		ms.Problem = "нет метрик VRAM"
	}
	return ms
}
