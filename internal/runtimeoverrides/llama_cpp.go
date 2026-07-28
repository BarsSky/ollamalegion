// Package runtimeoverrides — persistent runtime overrides для read-only
// config.json в bundled compose.
//
// Проблема (Session 17, 2026-07-27): bundled compose монтирует
// ../config:/app/config:ro. Это правильно с точки зрения безопасности
// (контейнер не перезаписывает host-файл), но WebUI-изменения не персистятся.
//
// Решение: runtime-оверрайды живут в writable named volume `balancer_data`
// (уже смонтирована в /app/data). При старте balancer:
//   1. Загружает /app/config/config.json (ro, host = source of truth)
//   2. Загружает /app/data/runtime-overrides/llama-cpp.json (rw, runtime changes)
//   3. Применяет оверрайды поверх base config
//   4. PUT llamaCpp пишет в override-файл, а не в main config
//
// Преимущества:
//   - host config.json неизменен (безопасно для перезапуска / шаринга)
//   - WebUI-изменения персистятся (переживают restart контейнера)
//   - "Reset to bundled defaults" = DELETE override-файл
//   - "docker compose down -v" стирает оверрайды (явный opt-in)
//
// Расширяемость: пакет спроектирован под несколько секций (balancing,
// backends и т.п.). Каждая секция имеет свой override-файл в /app/data/runtime-overrides/.
package runtimeoverrides

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

const (
	// DirName — поддиректория в /app/data для override-файлов.
	// Разделение от основного state.json позволяет не путать runtime-оверрайды
	// с runtime-state (running models, backends и т.п.).
	DirName = "runtime-overrides"

	// LlamaCppFileName — файл для оверрайдов llama.cpp.
	// Полная копия LlamaCppConfig (не partial), чтобы поведение было
	// предсказуемым: PUT form = полное состояние, override = полное состояние.
	LlamaCppFileName = "llama-cpp.json"
)

// ErrNotFound — оверрайдов нет (файл не существует). Не ошибка в строгом смысле.
// Deprecated: используется только внутри пакета для обратной совместимости.
// Новый API возвращает bool exists (см. LoadLlamaCpp).
var ErrNotFound = errors.New("runtime override not found")

// Store — точка доступа к runtime-оверрайдам. Thread-safe через RWMutex.
type Store struct {
	// dataDir — обычно /app/data (writable named volume).
	dataDir string

	mu sync.RWMutex
}

// New создаёт Store. dataDir должен существовать и быть writable.
// Если dataDir = "", оверрайды отключены (Load/Save/Clear возвращают
// ошибки, но IsEnabled() = false — handler'ы могут no-op).
func New(dataDir string) *Store {
	return &Store{dataDir: dataDir}
}

// IsEnabled — true, если Store сконфигурирован с writable директорией.
func (s *Store) IsEnabled() bool {
	return s.dataDir != ""
}

// llamaCppPath — полный путь к override-файлу.
func (s *Store) llamaCppPath() string {
	return filepath.Join(s.dataDir, DirName, LlamaCppFileName)
}

// LoadLlamaCpp — загружает override для llama.cpp.
//
// Контракт (см. api.OverridesStore интерфейс):
//   - (config, true, nil)  — override есть и загружен
//   - (nil, false, nil)    — override отсутствует (нормальная ситуация)
//   - (nil, false, err)    — ошибка ввода-вывода / парсинга
//
// Используется bool-флаг exists вместо sentinel-ошибки ErrNotFound,
// чтобы избежать циклических импортов между api и runtimeoverrides.
func (s *Store) LoadLlamaCpp() (*types.LlamaCppConfig, bool, error) {
	if !s.IsEnabled() {
		return nil, false, errors.New("runtimeoverrides: store not enabled (dataDir empty)")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.llamaCppPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read override file: %w", err)
	}

	var ll types.LlamaCppConfig
	if err := json.Unmarshal(data, &ll); err != nil {
		return nil, false, fmt.Errorf("parse override file: %w", err)
	}
	return &ll, true, nil
}

// SaveLlamaCpp — записывает override для llama.cpp. Создаёт директорию
// при необходимости. Идемпотентно: повторный вызов перезаписывает файл.
func (s *Store) SaveLlamaCpp(ll *types.LlamaCppConfig) error {
	if !s.IsEnabled() {
		return errors.New("runtimeoverrides: store not enabled (dataDir empty)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.dataDir, DirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create override dir: %w", err)
	}

	data, err := json.MarshalIndent(ll, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	path := s.llamaCppPath()
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write override file: %w", err)
	}
	logger.Get().Infow("runtimeoverrides: llamaCpp override saved",
		"path", path, "size_bytes", len(data))
	return nil
}

// ClearLlamaCpp — удаляет override-файл. После Clear Load() вернёт ErrNotFound.
// Используется для "Reset to bundled defaults" в WebUI.
func (s *Store) ClearLlamaCpp() error {
	if !s.IsEnabled() {
		return errors.New("runtimeoverrides: store not enabled (dataDir empty)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.llamaCppPath()
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove override file: %w", err)
	}
	logger.Get().Infow("runtimeoverrides: llamaCpp override cleared", "path", path)
	return nil
}

// HasLlamaCppOverride — быстрая проверка, есть ли файл. Полезно для UI
// (показать badge "Runtime overrides active").
func (s *Store) HasLlamaCppOverride() bool {
	if !s.IsEnabled() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, err := os.Stat(s.llamaCppPath())
	return err == nil
}

// ApplyLlamaCppToConfig — применяет override (если есть) к target.
//   - Если override отсутствует — target не изменяется (no-op).
//   - Если override есть — target.LlamaCpp заменяется целиком на override
//     (а не мерджится поле-в-поле). Это упрощает предсказуемость:
//     пользователь видит ровно то, что сохранил, без "сюрпризов"
//     со скрытыми полями из main config.
//
// Вызывается при старте balancer ПОСЛЕ загрузки config.json, чтобы
// runtime-изменения применялись сразу.
func (s *Store) ApplyLlamaCppToConfig(target *types.LoadBalancerConfig) error {
	ll, exists, err := s.LoadLlamaCpp()
	if err != nil {
		return err
	}
	if !exists {
		return nil // нет override — это нормально
	}
	target.LlamaCpp = *ll
	logger.Get().Infow("runtimeoverrides: applied llamaCpp override on startup",
		"contextLength", ll.ContextLength,
		"kvCacheType", ll.KVCacheType,
		"ropeScalingType", ll.RopeScalingType,
	)
	return nil
}
