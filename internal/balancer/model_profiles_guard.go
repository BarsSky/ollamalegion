package balancer

// R423 (2026-11-06): КАРТА ПРОФИЛЕЙ МОДЕЛЕЙ — ОБЩИЕ ДАННЫЕ, А НЕ «ПОЛЕ КОНФИГА».
//
// ЖИВОЙ ДЕФЕКТ (нашёл CI: `go test -race -tags llama_stub ./internal/balancer`):
//
//	WARNING: DATA RACE
//	Write at 0x... by goroutine ...:
//	  (*Proxy).SetModelProfile()  num_ctx_resolver.go
//	Previous read at ... by goroutine ...:
//	  (*Proxy).GetModelProfile()  num_ctx_resolver.go:161
//
// Профили моделей лежат в p.config.LlamaCppModelProfiles. Пишет их админка
// (PUT/DELETE /api/v1/cppworker/model-profiles, импорт config.json, загрузка
// profiles.json с диска), читают — горячие пути запроса: резолв окна,
// preflight n_ctx, выбор модели для загрузки, AutoTune, таймауты запроса. Это
// РАЗНЫЕ горутины (HTTP-хендлер админки и обработчик пользовательского
// запроса), поэтому «просто map» здесь — гонка. В Go гонка на map опаснее
// гонки на числе: рантайм может упасть с фатальной ошибкой
// "concurrent map read and map write" и унести с собой весь балансер.
//
// РЕШЕНИЕ. Один замок на прокси (profilesMu, см. proxy.go) и ТОЛЬКО снимки для
// чтения: читатель берёт копию карты под RLock, отпускает замок и дальше
// работает с копией. Под замком не выполняется НИЧЕГО, кроме копирования
// нескольких структур (никакой сети, диска, логов, вызовов наружу), поэтому
// дедлок невозможен: снимок нельзя «держать», он не является замком.
//
// Почему копия, а не RLock на время чтения: профилей единицы, копия дешевле
// любой случайной рекурсии (sync.RWMutex не рекурсивен — держащий RLock
// читатель, который возьмёт RLock снова при стоящем писателе, повиснет
// навсегда; см. R284 в proxy.go).

import "ollama-loadbalancer/pkg/types"

// profilesSnapshotR423 — копия карты per-model профилей под RLock.
// nil, если профилей нет (или прокси собран литералом без config — тесты).
func (p *Proxy) profilesSnapshotR423() map[string]types.LlamaCppModelProfile {
	if p == nil || p.config == nil {
		return nil
	}
	p.profilesMu.RLock()
	defer p.profilesMu.RUnlock()
	if len(p.config.LlamaCppModelProfiles) == 0 {
		return nil
	}
	out := make(map[string]types.LlamaCppModelProfile, len(p.config.LlamaCppModelProfiles))
	for name, profile := range p.config.LlamaCppModelProfiles {
		out[name] = profile
	}
	return out
}

// ModelProfilesSnapshot — публичный снимок для API (список профилей, AutoTune
// panel). Возвращает копию: вызывающая сторона может сколько угодно ходить по
// ней, не боясь гонки с админкой, которая в этот момент пишет профиль.
func (p *Proxy) ModelProfilesSnapshot() map[string]types.LlamaCppModelProfile {
	return p.profilesSnapshotR423()
}

// ReplaceModelProfiles — заменить карту профилей целиком (импорт config.json,
// загрузка profiles.json с диска). Импортированная карта НЕ удерживается:
// копируем, иначе чужой код мог бы менять её без замка.
func (p *Proxy) ReplaceModelProfiles(profiles map[string]types.LlamaCppModelProfile) {
	if p == nil || p.config == nil {
		return
	}
	var copyMap map[string]types.LlamaCppModelProfile
	if len(profiles) > 0 {
		copyMap = make(map[string]types.LlamaCppModelProfile, len(profiles))
		for name, profile := range profiles {
			copyMap[name] = profile
		}
	}
	p.profilesMu.Lock()
	defer p.profilesMu.Unlock()
	p.config.LlamaCppModelProfiles = copyMap
}

// UpdateModelProfile — атомарное «прочитать-изменить-записать» одной записи.
// Нужен админке AutoTune: она меняет одно поле профиля (per-model AutoTune), а
// не весь профиль; без замка конкурентный PUT профиля и PUT AutoTune затирали
// бы друг друга (последний писатель выигрывает целиком запись).
func (p *Proxy) UpdateModelProfile(modelName string, fn func(*types.LlamaCppModelProfile)) {
	if p == nil || p.config == nil || fn == nil {
		return
	}
	key := NormalizeModelProfileKey(modelName)
	p.profilesMu.Lock()
	defer p.profilesMu.Unlock()
	if p.config.LlamaCppModelProfiles == nil {
		p.config.LlamaCppModelProfiles = make(map[string]types.LlamaCppModelProfile)
	}
	profile := p.config.LlamaCppModelProfiles[key]
	fn(&profile)
	p.config.LlamaCppModelProfiles[key] = profile
}

// SwapConfigContentR423 — подменить содержимое config целиком (импорт
// config.json) под замком профилей.
//
// Зачем под замком: импорт присваивает весь LoadBalancerConfig, включая поле
// LlamaCppModelProfiles. Читатели карты берут её под RLock — значит и запись
// этого поля должна идти под тем же замком, иначе -race (и реальная гонка)
// остаётся, просто переезжает в другой файл.
//
// ЧЕСТНО О ГРАНИЦАХ: остальные поля config (backends, balancing, ...)
// импортируются по-прежнему без общего замка — это известное ограничение
// (импорт конфига — редкая админ-операция), и оно здесь не меняется. Данная
// функция закрывает ровно то, что закрывает R423: карту профилей.
func (p *Proxy) SwapConfigContentR423(imported *types.LoadBalancerConfig) {
	if p == nil || p.config == nil || imported == nil {
		return
	}
	p.profilesMu.Lock()
	defer p.profilesMu.Unlock()
	*p.config = *imported
}
