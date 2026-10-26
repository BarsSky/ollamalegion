package i18n

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RemoteSync — R234: подтягивание языка с балансера.
//
// ЗАЧЕМ. Балансер — источник истины для языка логов и ошибок: его переключают
// в WebUI (PUT /api/v1/i18n) или через env. Воркеры (cppworker, agent) живут в
// других контейнерах и о смене не знают, поэтому раз в interval спрашивают
// GET {balancer}/api/v1/i18n и применяют ответ. Пока балансер недоступен,
// действует локальная настройка (env или прежнее значение) — воркер не должен
// менять язык из-за сетевого сбоя.
type RemoteSync struct {
	baseURL  string
	token    string
	interval time.Duration
	client   *http.Client
	lastErr  string

	// R245: applyOnce вызывается и из фоновой горутины Start, и напрямую
	// (тесты/диагностика). Без мьютекса два вызова гоняли за lastErr, что
	// ловил -race в CI (TestRemoteSyncAppliesBalancerLanguage).
	mu sync.Mutex
}

// NewRemoteSync собирает синхронизатор. baseURL — адрес management-порта
// балансера (например http://loadbalancer:18081), token — X-API-Token.
func NewRemoteSync(baseURL, token string, interval time.Duration) *RemoteSync {
	return &RemoteSync{
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:    token,
		interval: interval,
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

// Start запускает синхронизацию в фоне. Первый запрос делается сразу (чтобы
// контейнер подхватил язык стенда при старте, не ожидая interval), дальше —
// по таймеру. Возвращаемый cancel останавливает горутину.
func (r *RemoteSync) Start(parent context.Context) (context.CancelFunc, error) {
	if r.baseURL == "" {
		return func() {}, fmt.Errorf("remote language sync: empty balancer URL")
	}
	if r.interval <= 0 {
		r.interval = 60 * time.Second
	}
	ctx, cancel := context.WithCancel(parent)
	go func() {
		r.applyOnce(ctx)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.applyOnce(ctx)
			}
		}
	}()
	return cancel, nil
}

// applyOnce делает один запрос и применяет язык, если он изменился.
func (r *RemoteSync) applyOnce(ctx context.Context) {
	// Сериализуем вызовы: запросы короткие (таймаут 5 c), зато lastErr и
	// применение языка не гоняются между горутиной Start и внешним вызовом.
	r.mu.Lock()
	defer r.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/api/v1/i18n", nil)
	if err != nil {
		r.noteError(err)
		return
	}
	if r.token != "" {
		req.Header.Set("X-API-Token", r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.noteError(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.noteError(fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}
	var body struct {
		Language string `json:"language"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		r.noteError(err)
		return
	}
	lang, auto, ok := Parse(body.Language)
	if !ok || auto {
		r.noteError(fmt.Errorf("unexpected language %q", body.Language))
		return
	}
	if lang != Get() {
		Set(lang)
		log.Print(T("i18n.remote.applied", lang))
	}
	r.lastErr = ""
}

// noteError логирует ошибку один раз на серию одинаковых: балансер может быть
// выключен, и воркер не должен забивать лог одной и той же строкой каждую минуту.
func (r *RemoteSync) noteError(err error) {
	msg := err.Error()
	if msg == r.lastErr {
		return
	}
	r.lastErr = msg
	log.Print(T("i18n.remote.unavailable", err))
}
