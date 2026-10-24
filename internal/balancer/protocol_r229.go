package balancer

// protocol_r229.go — R229 (2026-10-22): каким API-стилем клиент говорил.
//
// ЗАЧЕМ. Живые жалобы оператора («Cline жалуется на инструменты», «Open WebUI
// получает не тот n_ctx», «сессия висит») почти всегда относятся к одному из двух
// путей, а в панели пути были неразличимы: в сессиях и очереди видно клиента, но не
// видно, ходил он OpenAI-совместимым API (/v1/...) или родным Ollama (/api/...).
// В логах это можно восстановить только по path, а оператору нужно видеть в списке.
//
// Определение должно быть ОДНО (эта функция), иначе «openai» в сессиях и
// «OLLAMA» в очереди разъедутся.

import (
	"strings"
	"sync"
	"time"
)

const (
	// ProtocolOpenAI — OpenAI-совместимый путь: /v1/chat/completions, /v1/models, ...
	ProtocolOpenAI = "openai"
	// ProtocolOllama — родной Ollama путь: /api/chat, /api/generate, /api/tags, ...
	ProtocolOllama = "ollama"
)

// RequestProtocol — протокол по пути запроса. Пусто — путь не про инференс
// (health, метрики, внутренние админ-ручки): выдумывать протокол нельзя, иначе
// в сессиях появятся ложные бейджи.
func RequestProtocol(path string) string {
	p := strings.ToLower(strings.TrimSpace(path))
	switch {
	case p == "/v1" || strings.HasPrefix(p, "/v1/"):
		return ProtocolOpenAI
	case p == "/api" || strings.HasPrefix(p, "/api/"):
		// /api/v1/... — СВОЙ API балансера (админ/конфиг), а не инференс-путь
		// клиента: помечать его «Ollama» значило бы врать в бейдже.
		if strings.HasPrefix(p, "/api/v1/") || p == "/api/v1" {
			return ""
		}
		return ProtocolOllama
	}
	return ""
}

// protocolOfSession — протокол для очереди/монитора (пусто = неизвестен).
//
// Два источника, в порядке точности:
//  1. SessionManager — если ключ очереди совпадает с ID сессии;
//  2. admissionProtocols — ключ очереди (conv:/cid:/fp:), запомненный при захвате
//     слота: живая проверка показала, что это РАЗНЫЕ ключи, и без второго
//     источника waiting_detail.protocol оставался пустым.
func (p *Proxy) protocolOfSession(id string) string {
	if p == nil || strings.TrimSpace(id) == "" {
		return ""
	}
	if p.sessionMgr != nil {
		if proto := p.sessionMgr.ProtocolOf(id); proto != "" {
			return proto
		}
	}
	return p.admissionProto.get(id)
}

// protocolOfSession — протокол для элемента очереди: источник — менеджер сессий;
// nil-резолвер (тесты/инициализация) даёт пусто, а не панику.
func (aq *admissionQueue) protocolOfSession(session string) string {
	if aq == nil || aq.protocolOf == nil {
		return ""
	}
	return aq.protocolOf(session)
}
// admissionProtocols — R229: протокол по ключу admission-очереди.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ КАРТА. Ключ очереди (RequestSessionKey) — не ID сессии из
// SessionManager: живая проверка показала waiting_detail.session=`conv:...`, а в
// менеджере сессий такого ID нет, поэтому протокол в очереди оставался пустым.
// Ключ известен в момент захвата слота, там и запоминаем протокол; запись живёт
// ограниченно (TTL), чтобы карта не росла от каждого запроса.
type admissionProtocols struct {
	mu        sync.Mutex
	bySession map[string]admissionProtocolEntry
}

type admissionProtocolEntry struct {
	at       time.Time
	protocol string
}

const admissionProtocolTTL = 15 * time.Minute

func newAdmissionProtocols() *admissionProtocols {
	return &admissionProtocols{bySession: make(map[string]admissionProtocolEntry)}
}

func (a *admissionProtocols) note(session, protocol string) {
	if a == nil || session == "" || protocol == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	// Чистим раз в заметку, без таймеров: очередь короткая, карта маленькая.
	for k, v := range a.bySession {
		if now.Sub(v.at) > admissionProtocolTTL {
			delete(a.bySession, k)
		}
	}
	a.bySession[session] = admissionProtocolEntry{at: now, protocol: protocol}
}

func (a *admissionProtocols) get(session string) string {
	if a == nil || session == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.bySession[session]
	if !ok || time.Since(v.at) > admissionProtocolTTL {
		return ""
	}
	return v.protocol
}

// noteAdmissionProtocol — запомнить протокол для ключа admission-очереди.
func (p *Proxy) noteAdmissionProtocol(session, protocol string) {
	if p == nil || p.admissionProto == nil {
		return
	}
	p.admissionProto.note(session, protocol)
}