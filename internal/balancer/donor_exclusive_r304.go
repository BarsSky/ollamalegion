package balancer

// donor_exclusive_r304.go — R304 (2026-10-30): донор держит ТОЛЬКО память.
//
// ДОКТРИНА ОПЕРАТОРА. «Если бэкенд становится донором, он уже не может подымать
// свои копии моделей — он становится именно донором мастера и может только
// делить память с мастером». Это не оптимизация, а смысл роли: VRAM донора
// целиком отдана слоям чужой модели через rpc-server. Своя копия на том же узле
// отнимает у мастера ровно те гигабайты, ради которых донора и назначили.
//
// ЧТО БЫЛО НЕ ТАК ДО ЭТОЙ ПРАВКИ (живой класс дефектов, не гипотеза).
// Роль donor описана в types.BackendRoleDonor как «отдаёт свою GPU через
// rpc-server», но НИ ОДИН путь выбора бэкенда её не проверял: единственным
// фильтром «этот узел не держит модель целиком» был isGroupWorkerR92 (воркер
// логического бэкенда из balancing.backendGroups[]). Роль донора и участие в
// группе — РАЗНЫЕ вещи, и вторая из них в обычном развёртывании не настроена.
//
// Следствия, которые наблюдались живьём:
//   - «второй агент вырвал модель у первого»: селектор выбирал узел, который
//     оператор только что назначил донором (или наоборот — донор продолжал
//     обслуживать копию, пока мастер грузил в его VRAM слои, и загрузка падала
//     по памяти);
//   - модель грузилась НА донор, хотя по роли он должен только отдавать память:
//     оператор видел в панели «донор», а в списке моделей — загруженную копию;
//   - перезагрузка n_ctx выбирала донора целью и тратила его VRAM впустую.
//
// ЧТО ЗДЕСЬ СДЕЛАНО. Один предикат на весь горячий путь — cannotHostModelR304:
// «узел не имеет права держать копию модели». Он объединяет воркера раскладки
// (R92) и донора (R118/R304), и его обязаны спрашивать все места выбора. Роли
// читаются из НЕИЗМЕНЯЕМОГО снимка под atomic.Pointer, а не через GetBackend:
// горячий путь уже держит p.mu.RLock, и вложенный RLock при ожидающем писателе —
// это ровно дедлок R284 (см. docs, заморозка балансера 2026-10-29).
//
// ЧЕГО ЭТО НЕ ДЕЛАЕТ. Не выгружает самовольно модель у донора и не меняет роль
// оператора. Факт «донор держит копию» (осталась с прежней роли либо загружена
// в обход) честно называется в API (donor.ownCopies) и в логе: решение выгружать
// чужую работу принимает оператор или путь загрузки, а не фоновая уборка.
// Флаг LB_DONOR_ALLOW_OWN_COPIES (по умолчанию 0) возвращает прежнее поведение
// целиком — тем же одним предикатом, чтобы «выключено» не оставляло дыр.

import (
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// donorCopyLogR304 — ограничитель: «донор держит свою копию» не чаще раза в
// 5 минут на узел. Тик гарда раскладки — 15–20 с, без ограничителя это полотно
// одинаковых строк (класс R299).
var donorCopyLogR304 = newLogRateLimiter(5 * time.Minute)

// donorAllowOwnCopiesR304 — оператор разрешил донорам держать свои копии.
//
// ПОЧЕМУ ФЛАГ-ИСКЛЮЧЕНИЕ, А НЕ ФЛАГ-ВОЗМОЖНОСТЬ. Доктрина — «донор только
// память», поэтому по умолчанию запрещено. Исключение нужно для одного
// практического случая: оператор ставит роль донора на узел, который продолжает
// обслуживать уже идущую работу, и хочет дожить до конца стримов без обрыва.
func donorAllowOwnCopiesR304() bool {
	if v, ok := settings.HotInt("LB_DONOR_ALLOW_OWN_COPIES"); ok {
		return v != 0
	}
	return false
}

// donorIndexR304 — неизменяемый снимок ролей раскладки.
//
// ЗАЧЕМ СНИМОК, А НЕ ЗАПРОС К p.backends. Предикат нужен в горячем пути и в
// местах, где p.mu уже удерживается на чтение. Обращение к карте без блокировки
// — гонка, а вложенный RLock — дедлок R284. Снимок снимает оба вопроса: он
// подменяется целиком (atomic.Pointer), читается без локов и не бывает
// полу-built.
type donorIndexR304 struct {
	// masterID — главный бэкенд раскладки ("" — не назначен).
	masterID string
	// donors — донор -> мастер ("" = мастер не назначен; роль уже донорская).
	donors map[string]string
}

// isDonor — узел имеет роль донора.
func (idx *donorIndexR304) isDonor(backendID string) bool {
	if idx == nil {
		return false
	}
	_, ok := idx.donors[backendID]
	return ok
}

// rebuildDonorIndexR304 — пересобрать снимок ролей.
//
// Вызывается: при смене роли (SetBackendRole), при построении реестра групп
// (старт/перезагрузка конфига) и на каждом тике гарда раскладки. Тик не лишний:
// роль может прийти из persistent-оверрайдов после старта, а забыть вызов —
// ровно тот класс «новая точка записи обошла общий путь».
func (p *Proxy) rebuildDonorIndexR304() {
	if p == nil {
		return
	}
	p.mu.RLock()
	idx := p.buildDonorIndexLockedR304()
	p.mu.RUnlock()
	p.donorIdxR304.Store(idx)
}

// buildDonorIndexLockedR304 — снимок из текущего реестра (p.mu удерживается).
//
// Детерминизм: при нескольких главных берём наименьший ID, а не «какой попался
// в карте» — иначе два одинаковых запроса подряд показывали бы разные адреса
// мастера (класс R91, пинг-понг).
func (p *Proxy) buildDonorIndexLockedR304() *donorIndexR304 {
	idx := &donorIndexR304{donors: map[string]string{}}
	masters := make([]string, 0, 1)
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if strings.TrimSpace(state.Backend.Role) == types.BackendRoleMain {
			masters = append(masters, id)
		}
	}
	if len(masters) > 0 {
		sort.Strings(masters)
		idx.masterID = masters[0]
	}
	for id, state := range p.backends {
		if state == nil || state.Backend == nil {
			continue
		}
		if strings.TrimSpace(state.Backend.Role) != types.BackendRoleDonor {
			continue
		}
		master := ""
		if id != idx.masterID {
			master = idx.masterID
		}
		idx.donors[id] = master
	}
	return idx
}

// donorIndex — текущий снимок ролей (неизменяемый; nil безопасен).
func (p *Proxy) donorIndex() *donorIndexR304 {
	if p == nil {
		return nil
	}
	return p.donorIdxR304.Load()
}

// isDonorR304 — у этого бэкенда РОЛЬ донора (факт, без флага разрешения).
//
// Роль — единственный источник: «у него случайно поднят rpc-server» донором не
// делает, иначе ручной тумблер на главном выкидывал бы главного из выбора.
func (p *Proxy) isDonorR304(backendID string) bool {
	return p.donorIndex().isDonor(backendID)
}

// donorMasterIDR304 — какой мастеру принадлежит донор ("" — не назначен).
func (p *Proxy) donorMasterIDR304(donorID string) string {
	idx := p.donorIndex()
	if idx == nil {
		return ""
	}
	return idx.donors[donorID]
}

// cannotHostModelR304 — узел не имеет права держать СВОЮ копию модели.
//
// Это единственная точка, которую обязаны спрашивать все селекторы и пути
// загрузки. Воркер раскладки (R92) не держит модель целиком по построению;
// донор (R304) — потому что его VRAM отдана мастеру.
func (p *Proxy) cannotHostModelR304(backendID string) bool {
	if p.isGroupWorkerR92(backendID) {
		return true
	}
	return p.donorHostsForbiddenR304(backendID)
}

// donorHostsForbiddenR304 — донору запрещены свои копии (с учётом флага).
func (p *Proxy) donorHostsForbiddenR304(backendID string) bool {
	if !p.isDonorR304(backendID) {
		return false
	}
	return !donorAllowOwnCopiesR304()
}

// DonorMarkR304 — пометка донора для API и панели.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ОБЪЕКТ, А НЕ ПОЛЕ role. Роль отвечает на вопрос «что решил
// оператор», а панели нужно ответить ещё на три: чей это донор, может ли он
// грузить свои модели и держит ли он их сейчас вопреки роли. Без последнего
// пометка «донор» рядом с загруженной копией выглядит как противоречие в
// интерфейсе, а не как нарушение инварианта.
type DonorMarkR304 struct {
	// Donor — роль донора (то же, что role == "donor"; отдельным полем, чтобы
	// панель не собирала смысл из строки).
	Donor bool `json:"donor"`
	// MasterBackendID/MasterHost — чей это донор ("" — мастер не назначен).
	MasterBackendID string `json:"masterBackendId,omitempty"`
	MasterHost      string `json:"masterHost,omitempty"`
	// CanHostOwnModels — может ли узел поднимать СВОИ копии моделей.
	CanHostOwnModels bool `json:"canHostOwnModels"`
	// OwnCopies — модели, которые у него сейчас загружены/грузятся (нарушение).
	OwnCopies []string `json:"ownCopies,omitempty"`
	// Note — человекочитаемая причина запрета (для подсказки в панели).
	Note string `json:"note,omitempty"`
}

// DonorMarkR304 — собрать пометку по бэкенду.
func (p *Proxy) DonorMarkR304(backendID string) DonorMarkR304 {
	mark := DonorMarkR304{CanHostOwnModels: true}
	if p == nil || backendID == "" {
		return mark
	}
	mark.Donor = p.isDonorR304(backendID)
	if p.isGroupWorkerR92(backendID) {
		// Воркер раскладки (R92) — другой механизм, но ответ на вопрос тот же:
		// своих копий у него быть не может.
		mark.CanHostOwnModels = false
		if mark.Note == "" {
			mark.Note = i18n.T("balancer.donor.note_layout_worker")
		}
	}
	if mark.Donor {
		mark.MasterBackendID = p.donorMasterIDR304(backendID)
		if b := p.GetBackend(mark.MasterBackendID); b != nil {
			mark.MasterHost = b.Host
		}
		mark.CanHostOwnModels = donorAllowOwnCopiesR304()
		if mark.MasterBackendID != "" {
			mark.Note = i18n.T("balancer.donor.note_with_master", mark.MasterBackendID)
		} else {
			mark.Note = i18n.T("balancer.donor.note_no_master")
		}
		mark.OwnCopies = p.ownCopiesOnBackendR304(backendID)
	}
	return mark
}

// ownCopiesOnBackendR304 — модели, загруженные или грузящиеся на узле.
//
// Источник — снапшот поллера (тот же, по которому принимается решение о копиях):
// ходить в сеть из отчёта панели нельзя, а «есть ли копия» обязано совпадать с
// тем, что видит селектор.
func (p *Proxy) ownCopiesOnBackendR304(backendID string) []string {
	if p == nil || p.metricsMgr == nil || backendID == "" {
		return nil
	}
	lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics(backendID)
	if !ok || lm == nil {
		return nil
	}
	out := make([]string, 0, len(lm.LoadedModels)+len(lm.LoadingModels))
	for _, m := range lm.LoadedModels {
		if name := strings.TrimSpace(m.Name); name != "" {
			out = append(out, name)
		}
	}
	for _, m := range lm.LoadingModels {
		if name := strings.TrimSpace(m.Name); name != "" {
			out = append(out, name+" (loading)")
		}
	}
	sort.Strings(out)
	return out
}

// noteDonorOwnCopiesR304 — записать в лог, что донор вопреки роли держит копию.
//
// Одна строка на узел не чаще раза в 5 минут: это диагностика состояния, а не
// событие запроса. Возвращает true, если сообщение прошло (чтобы вызывающий не
// считал это решением — решение принимает оператор).
func (p *Proxy) noteDonorOwnCopiesR304(backendID string, copies []string) {
	if len(copies) == 0 || donorAllowOwnCopiesR304() {
		return
	}
	if ok, dropped := donorCopyLogR304.allow(backendID); ok {
		logger.Get().Warnw("donor holds its own model copies (R304: donor must only serve the master's memory)",
			"backend", backendID,
			"master", p.donorMasterIDR304(backendID),
			"own_copies", strings.Join(copies, ", "),
			"suppressed_before", dropped)
	}
}
