// donor_role_r304.go — R304 (2026-10-30): «донор держит только память».
//
// ДОКТРИНА ОПЕРАТОРА (дословно): «Если бэкенд становится донором, он уже не может
// подымать свои копии моделей — он становится именно донором мастера и может
// только делить память с мастером.»
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ПРЕДИКАТ, А НЕ ПРОВЕРКА В ОБРАБОТЧИКЕ. Решение «этой машине
// можно держать свою копию модели?» принимается в четырёх разных местах (явная
// загрузка и reload по HTTP, ленивая загрузка по запросу чата, авто-загрузка при
// старте, /api/models для панели). Четыре копии правил неизбежно разъезжаются —
// ровно так в этом же коде уже появлялись «две правды» про rpcEnabled (живой
// дефект, из-за которого появился SetRPCConfig). Поэтому правило живёт здесь
// одно, а вызывающие только спрашивают.
package cppbackend

import (
	"sort"
	"strings"
)

// Роли бэкенда в раскладке. Пустая строка = роль НЕИЗВЕСТНА (балансер её ещё не
// объявлял) и трактуется как «не донор»: воркер не вправе запрещать загрузку по
// догадке — иначе обновление воркера без балансера останавливало бы работу.
const (
	RoleStandalone = "standalone"
	RoleMain       = "main"
	RoleDonor      = "donor"
)

// Причины донорства. Машиночитаемые: уезжают в ответ 409 и в /api/models, чтобы
// панель и балансер объясняли запрет, а не показывали «просто ошибку».
const (
	// DonorReasonRole — роль объявлена приказом балансера (backendRole=donor).
	DonorReasonRole = "role=donor"
	// DonorReasonRPCServerEnabled — на этой машине поднят узел раскладки.
	//
	// ПОЧЕМУ ЭТО ТОЖЕ ДОНОР, А НЕ ДОГАДКА. Балансер включает свой узел ТОЛЬКО у
	// роли donor, а оператор, поднявший узел руками (docs/operations/donor-update.md,
	// галочка «узел раскладки» в WebUI), сделал машину донором тем же действием:
	// узел принимает соединения мастера и отдаёт ему СВОЮ память. Это честный
	// факт состояния процесса, а не умозаключение: узел слушает здесь и сейчас.
	// Разрешить такой машине ещё и грузить своё — значит отдать одну и ту же
	// память дважды (мастер уже планирует по ней раскладку).
	DonorReasonRPCServerEnabled = "rpc_server_enabled"
)

// BackendRoles — допустимые значения Config.BackendRole (порядок — для сообщений
// валидатора и для панели).
func BackendRoles() []string {
	return []string{"", RoleStandalone, RoleMain, RoleDonor}
}

// NormalizeBackendRole — привести присланное значение к одному из BackendRoles.
//
// ЗАЧЕМ НОРМАЛИЗАЦИЯ. Приказ приходит по HTTP, и «Donor», « donor » — та же роль.
// Отвергать их как невалидные значило бы показать оператору отказ валидации на
// ровном месте (и «настройка не применилась» — знакомый класс жалоб). Возвращает
// (нормализованное значение, известно ли оно).
func NormalizeBackendRole(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	for _, allowed := range BackendRoles() {
		if v == allowed {
			return v, true
		}
	}
	return "", false
}

// DonorMark — состояние донорства воркера: одна пометка на всё приложение.
//
// Все поля сериализуемы: снимок уезжает в /api/models (панель показывает по нему
// «донор» и блокирует кнопки загрузки) и в ответ 409.
type DonorMark struct {
	// Role — объявленная роль (standalone|main|donor|"" = неизвестна).
	Role string `json:"role"`
	// Donor — эта машина работает как донор мастера (по роли ИЛИ по факту узла).
	Donor bool `json:"donor"`
	// CanHostOwnModels — вправе ли машина поднимать СВОИ копии моделей.
	CanHostOwnModels bool `json:"canHostOwnModels"`
	// Reason — почему запрет: role=donor | rpc_server_enabled (пусто = запрета нет).
	Reason string `json:"reason,omitempty"`
	// OwnCopies — модели, загруженные/грузящиеся ВОПРЕКИ роли: копии, которые
	// донору придётся выгрузить. Пусто у не-донора: там копия роли не противоречит.
	OwnCopies []string `json:"ownCopies,omitempty"`
}

// donorStateR304 — сырьё правила: объявленная роль и поднят ли свой узел.
//
// Читается под b.mu: SetBackendRole/SetRPCServerEnabled меняют эти поля на ходу
// (приказ балансера), а пометку читают обработчики в других горутинах.
func (b *Backend) donorStateR304() (role string, nodeUp bool) {
	if b == nil {
		return "", false
	}
	b.mu.RLock()
	role = strings.TrimSpace(b.cfg.BackendRole)
	nodeUp = b.cfg.RPCServerEnabled
	b.mu.RUnlock()
	return role, nodeUp
}

// DonorOnly — (запрещено ли держать свои копии, причина).
//
// ПОРЯДОК ПРАВИЛ ИМЕННО ТАКОЙ, И ВОТ ПОЧЕМУ:
//
//  1. role == "donor" → причина role=donor. Это ПРИКАЗ, он старше любого факта:
//     даже если узел ещё не поднялся (перезапуск, порт занят), машина уже донор —
//     иначе копия успела бы подняться в окне между приказом и стартом узла.
//  2. иначе поднят свой узел (rpcServerEnabled) → причина rpc_server_enabled.
//     Машина отдаёт память мастеру, значит своих копий не поднимает — даже если
//     роль объявлена как standalone (её мог включить оператор руками).
//  3. иначе — не донор, копии разрешены. Пустая/неизвестная роль НЕ запрещает:
//     запрет по «мы не знаем роли» остановил бы воркер, обновлённый раньше
//     балансера.
func (b *Backend) DonorOnly() (bool, string) {
	role, nodeUp := b.donorStateR304()
	switch {
	case role == RoleDonor:
		return true, DonorReasonRole
	case nodeUp:
		return true, DonorReasonRPCServerEnabled
	}
	return false, ""
}

// DonorMark — полная пометка донора для отчёта и для отказов.
//
// OwnCopies заполняется ТОЛЬКО у донора: у не-донора загруженная модель роли не
// противоречит, и «вопиющая копия» там была бы ложным сигналом тревоги.
func (b *Backend) DonorMark() DonorMark {
	// Состояние читается ОДИН раз: пометка обязана быть непротиворечивой (role и
	// nodeUp из одного снимка), иначе между двумя чтениями приказ успел бы
	// сменить роль и в ответ уехала бы смесь двух состояний.
	role, nodeUp := b.donorStateR304()
	mark := DonorMark{Role: role, CanHostOwnModels: true}
	switch {
	case role == RoleDonor:
		mark.Donor, mark.CanHostOwnModels, mark.Reason = true, false, DonorReasonRole
	case nodeUp:
		mark.Donor, mark.CanHostOwnModels, mark.Reason = true, false, DonorReasonRPCServerEnabled
	}
	if mark.Donor {
		mark.OwnCopies = b.ownCopiesR304()
	}
	return mark
}

// ownCopiesR304 — модели в состоянии loaded/loading на этой машине.
//
// Источник списка — ListModels(): из него же строит ответ /api/models, и второй
// перебор b.models разошёлся бы с тем, что видит оператор в панели (например по
// state, который выставляется в разных ветках загрузки).
func (b *Backend) ownCopiesR304() []string {
	if b == nil {
		return nil
	}
	models := b.ListModels()
	out := make([]string, 0, len(models))
	for _, m := range models {
		if m.State == StateLoaded || m.State == StateLoading {
			out = append(out, m.Name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	// Сортировка — чтобы ответ не «дрожал» между опросами: список строится из
	// map, порядок обхода которой случаен, а панель сравнивает снимки.
	sort.Strings(out)
	return out
}

// SetBackendRole — донести объявленную роль до ЖИВОГО конфига бэкенда.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ МЕТОД. `PUT /api/v1/cppworker/config/update` пишет в
// `currentConfig` (объект пакета cppworker), а решения принимает `b.cfg` —
// РАЗНЫЕ объекты. Ровно на этом уже терялся rpcEnabled (см. SetRPCConfig):
// ответ API говорил «применено», а бэкенд продолжал работать по старому. Без
// этого метода строка «роль = donor» осталась бы в ответе и в файле, а загрузку
// бэкенд по-прежнему разрешал.
func (b *Backend) SetBackendRole(role string) {
	if b == nil {
		return
	}
	role = strings.TrimSpace(role)
	if norm, known := NormalizeBackendRole(role); known {
		role = norm
	}
	b.mu.Lock()
	b.cfg.BackendRole = role
	b.mu.Unlock()
}

// SetRPCServerEnabled — донести ФАКТ «свой узел включён» до живого конфига.
//
// ЗАЧЕМ. Узел, включённый приказом балансера, жил только в `currentConfig` и в
// супервизоре; `b.cfg` о нём не знал. По правилу 2 (см. DonorOnly) поднятый узел
// — признак донора, поэтому без переноса «донор по факту узла» начинал бы
// действовать лишь после пересоздания контейнера, а между приказом и
// пересозданием машина грузила бы своё, уже отдавая память мастеру.
func (b *Backend) SetRPCServerEnabled(enabled bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.cfg.RPCServerEnabled = enabled
	b.mu.Unlock()
}
