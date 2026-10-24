package balancer

// rpc_layout_override_r229.go — R229 (2026-10-22): тумблер раскладки, который
// ПРИНАДЛЕЖИТ ОПЕРАТОРУ, а не гарду.
//
// ЖИВОЙ ДЕФЕКТ (воспроизведён на стенде перед правкой). Оператор выключает
// раскладку через PUT /api/v1/cppworker/config/update {"rpcEnabled": false} —
// воркер отвечает `applied:["rpcEnabled","rpcServers(cleared)"]`, всё честно.
// Через ~15 секунд балансерский EnsureRPCLayout (тик ролей) возвращает
// `rpcEnabled: true` со списком доноров, потому что роль главного и живой донор
// означают «раскладка должна быть включена». В WebUI это выглядело как
// «тумблер сам включается обратно»; следующая загрузка снова уходила на донора.
//
// РЕШЕНИЕ. Выбор оператора хранится в записи бэкенда (persistent: state.json) и
// побеждает роль. Гард по-прежнему проверяет доступность узлов и НЕ отдаёт
// загрузке мёртвый адрес (иначе llama.cpp падает SIGABRT), но состав «включено/
// выключено» берёт из этого решения. «Авто» (nil) возвращает прежнее поведение.

// SetRPCEnabledOverride — сохранить решение оператора: enabled != nil (on/off) или
// nil (авто по ролям). Возвращает false, если бэкенд не найден.
//
// Побочный эффект: сбрасывает кэш «уже применено», иначе EnsureRPCLayout увидел
// бы совпавший ключ и не отправил новое значение воркеру до следующего тика.
func (p *Proxy) SetRPCEnabledOverride(backendID string, enabled *bool) bool {
	if p == nil || backendID == "" {
		return false
	}
	p.mu.Lock()
	state, ok := p.backends[backendID]
	if !ok || state == nil || state.Backend == nil {
		p.mu.Unlock()
		return false
	}
	if enabled == nil {
		state.Backend.RPCEnabledOverride = nil
	} else {
		v := *enabled
		state.Backend.RPCEnabledOverride = &v
	}
	if p.rpcLayoutApplied != nil {
		delete(p.rpcLayoutApplied, backendID)
	}
	p.mu.Unlock()
	p.scheduleSave()
	return true
}

// RPCEnabledOverrideMode — "auto" | "on" | "off" для API/WebUI.
func (p *Proxy) RPCEnabledOverrideMode(backendID string) string {
	if p == nil {
		return "auto"
	}
	b := p.GetBackend(backendID)
	if b == nil || b.RPCEnabledOverride == nil {
		return "auto"
	}
	if *b.RPCEnabledOverride {
		return "on"
	}
	return "off"
}

// ReportedRPCLayout — что cppworker СООБЩАЕТ о своей раскладке (rpcEnabled +
// активные узлы). known=false — воркер ещё не присылал метрик; в этом случае
// вызывающий не должен делать вывод «значение не применилось».
func (p *Proxy) ReportedRPCLayout(backendID string) (enabled bool, servers []string, known bool) {
	if p == nil {
		return false, nil, false
	}
	return p.cppworkerRPCLayout(backendID)
}
