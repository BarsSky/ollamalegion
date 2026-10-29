package balancer

// active_streams_r281.go — R281 (2026-10-29): операторский reload из WebUI/API
// подчиняется тому же правилу «не рвать чужой стрим», что и авто-перезагрузка
// (R271/R280).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Это единственный мостик из api-пакета в учёт стримов:
// api не может звать неэкспортированные waitStreamsIdleR271/noteDestructiveOpR271,
// а дублировать ожидание в api-пакете нельзя — ровно так в этой серии появлялись
// «вторые пути», которые потом обходили общий механизм (учёт стримов, фильтр
// меток, границы SSE).
//
// Здесь только обёртки; сам учёт живёт в active_streams_r271.go.

import "context"

// WaitStreamsIdleBeforeDestructive — публичная обёртка для слоя API (R281).
//
// Зачем обёртка: reload, который оператор запускает из WebUI/API, обязан
// подчиняться тому же правилу R271, что и авто-перезагрузка, но
// waitStreamsIdleR271 неэкспортирован. Дублировать ожидание в api-пакете
// нельзя — это ровно тот «второй путь», из-за которого мы уже ловили
// расхождения (учёт стримов, фильтр меток, границы SSE).
func (p *Proxy) WaitStreamsIdleBeforeDestructive(ctx context.Context, backendID, model string) bool {
	// R291: операторская кнопка реагирует сразу. Решение «ждать/отложить»
	// принимает общая функция, чтобы ручной reload не расходился с авто-путями.
	ok, _ := p.deferDestructiveOpR291(ctx, backendID, model, "operator-reload")
	return ok
}

// NoteDestructiveReload — отметить, что мы перезагружаем модель операторским
// запросом (R281). Нужно, чтобы оборванный этим стрим получил причину
// balancer_reload, а не «upstream ended».
func (p *Proxy) NoteDestructiveReload(backendID, model string) {
	p.noteDestructiveOpR271(backendID, model, destructiveReloadR271)
}

// ActiveStreamsOnBackend — для API-слоя: сколько стримов идёт на бэкенде
// (в тексте отказа оператору нужно число, а не «deferred»).
func (p *Proxy) ActiveStreamsOnBackend(backendID string) int {
	return p.activeStreamsOnBackend(backendID)
}
