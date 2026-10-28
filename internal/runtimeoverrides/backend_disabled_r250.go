package runtimeoverrides

// backend_disabled_r250.go — R250 (2026-10-28): персист флага «не использовать
// бэкенд» (решение оператора).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ, А НЕ ПОЛЕ В state.json. Ровно та же причина, что у ролей
// (backend_roles_r118.go): в bundled-развёртывании config.json смонтирован
// read-only, а запись бэкенда ПЕРЕСОБИРАЕТСЯ при саморегистрации агента и
// heartbeat. Флаг, который живёт только в записи, терялся бы при первой же
// перерегистрации (наблюдалось с ролями: «после рестарта главный остался без
// доноров»). Здесь решение оператора лежит в writable-томе
// `balancer_data:/app/data/runtime-overrides/`, а балансер периодически приводит
// записи к нему (см. api.reapplyBackendDisabledOnce).
//
// СЕМАНТИКА КАРТЫ. Запись есть → это ЯВНОЕ решение оператора (и «выкл», и
// «вкл»). Именно поэтому храним оба состояния, а не только выключенные: после
// нажатия «вернуть в работу» запись с disabled=false обязана перебить флаг,
// который мог остаться в state.json от прошлой сессии.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	// BackendDisabledFileName — файл флагов: {"<backendID>": {"disabled":true,...}}.
	BackendDisabledFileName = "backend-disabled.json"
)

// BackendDisabledRecord — решение оператора по одному бэкенду.
//
// Причина и момент хранятся рядом с флагом, а не в логах: через неделю оператор
// смотрит в панель и должен видеть «выключен 28.10, причина: плановое
// обслуживание», а не пустой квадратик.
type BackendDisabledRecord struct {
	Disabled bool   `json:"disabled"`
	Reason   string `json:"reason,omitempty"`
	Since    string `json:"since,omitempty"` // RFC3339 UTC
}

// backendDisabledPath — полный путь к файлу флагов.
func (s *Store) backendDisabledPath() string {
	return filepath.Join(s.dataDir, DirName, BackendDisabledFileName)
}

// LoadBackendDisabled — флаги из sidecar-файла.
//
// Контракт как у LoadBackendRoles:
//   - (records, true, nil)  — файл есть и разобран (возможно, пустая карта);
//   - (nil, false, nil)     — файла нет (нормальная ситуация, оператор не
//     выключал бэкенды);
//   - (nil, false, err)     — ошибка ввода-вывода / парсинга.
func (s *Store) LoadBackendDisabled() (map[string]BackendDisabledRecord, bool, error) {
	if s == nil || !s.IsEnabled() {
		return nil, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.backendDisabledPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	records := map[string]BackendDisabledRecord{}
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, false, err
	}
	return records, true, nil
}

// SaveBackendDisabled — записать флаги (атомарно: tmp + rename).
func (s *Store) SaveBackendDisabled(records map[string]BackendDisabledRecord) error {
	if s == nil || !s.IsEnabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Join(s.dataDir, DirName), 0o755); err != nil {
		return err
	}
	if records == nil {
		records = map[string]BackendDisabledRecord{}
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	path := s.backendDisabledPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NewBackendDisabledRecord — запись решения «выключить» с текущим моментом.
func NewBackendDisabledRecord(reason string) BackendDisabledRecord {
	return BackendDisabledRecord{
		Disabled: true,
		Reason:   reason,
		Since:    time.Now().UTC().Format(time.RFC3339),
	}
}
