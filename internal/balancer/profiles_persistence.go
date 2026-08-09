// profiles_persistence.go — отдельная персистенция для model profiles (Round 31).
//
// Проблема (2026-08-09): s.configSaver() пишет в config.json. В bundled compose
// `../config:/app/config:ro` — read-only mount. PUT /api/v1/cppworker/model-profiles/{name}
// тихо логирует "failed to save config to disk" и in-memory изменение теряется
// при рестарте. Юзеру приходится re-PUT каждый раз.
//
// Решение: персистим профили ОТДЕЛЬНО в /app/data/profiles.json (writable named volume).
//   - При старте LoadProfilesFromFile() мержит с config.json (config приоритет
//     если есть коллизии — config это "bundled defaults").
//   - При PUT/DELETE/apply — SaveProfilesToFile().
//   - Формат: { "version": 1, "profiles": { "gemma-4": {...}, ... } }.
//
// Fallback: если /app/data/profiles.json не доступен для записи — log error
// и продолжить работу (in-memory сохраняется до следующего рестарта).

package balancer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// profilesFileFormat — структура файла /app/data/profiles.json.
// v1: исходная версия (Round 31).
type profilesFileFormat struct {
	Version  int                                  `json:"version"`
	Profiles map[string]types.LlamaCppModelProfile `json:"profiles"`
}

// profilesFileVersion — current schema version
const profilesFileVersion = 1

// profilesFileMu — mutex для concurrent access к profiles.json
// (SaveProfilesToFile может вызываться из нескольких handler'ов одновременно)
var profilesFileMu sync.Mutex

// profilesFilePath вычисляет путь к profiles.json на основе statePath.
// statePath обычно "data/state.json" → profiles.json в той же директории.
// Если statePath пустой — возвращается просто "data/profiles.json".
func profilesFilePath(statePath string) string {
	if statePath == "" {
		return "data/profiles.json"
	}
	dir := filepath.Dir(statePath)
	return filepath.Join(dir, "profiles.json")
}

// LoadProfilesFromFile — загружает профили из файла.
//
// Мержит с текущим config.LlamaCppModelProfiles (config имеет приоритет —
// это "bundled defaults" из config.json, который mount'ится :ro).
//
// Возвращает (loaded, error). loaded = количество профилей добавленных из файла.
// Если файл не существует — возвращает (0, nil), не ошибка.
func (p *Proxy) LoadProfilesFromFile() (int, error) {
	if p.config == nil {
		return 0, nil
	}
	path := profilesFilePath(p.statePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Get().Debugw("LoadProfilesFromFile: profiles file not found, skipping",
				"path", path)
			return 0, nil
		}
		return 0, fmt.Errorf("read profiles file: %w", err)
	}

	var file profilesFileFormat
	if err := json.Unmarshal(data, &file); err != nil {
		return 0, fmt.Errorf("unmarshal profiles file: %w", err)
	}
	if file.Version != profilesFileVersion {
		logger.Get().Warnw("LoadProfilesFromFile: unknown version, skipping",
			"path", path, "version", file.Version, "expected", profilesFileVersion)
		return 0, nil
	}

	if p.config.LlamaCppModelProfiles == nil {
		p.config.LlamaCppModelProfiles = make(map[string]types.LlamaCppModelProfile)
	}

	loaded := 0
	for name, profile := range file.Profiles {
		// Skip if config.json already has this profile (config wins — bundled defaults)
		if _, exists := p.config.LlamaCppModelProfiles[name]; exists {
			logger.Get().Debugw("LoadProfilesFromFile: config has higher priority, skipping",
				"model", name)
			continue
		}
		p.config.LlamaCppModelProfiles[name] = profile
		loaded++
	}
	logger.Get().Infow("LoadProfilesFromFile: loaded profiles from disk",
		"path", path, "loaded", loaded, "total_in_file", len(file.Profiles))
	return loaded, nil
}

// SaveProfilesToFile — сохраняет ВСЕ профили в файл (atomic write).
//
// Использует tmp file + rename для atomic write (избегаем corruption
// при concurrent PUT + crash).
//
// Возвращает error если запись не удалась. Не блокирует — caller решает
// что делать (in-memory profile сохранён, просто нет persistence).
func (p *Proxy) SaveProfilesToFile() error {
	if p.config == nil {
		return fmt.Errorf("nil config")
	}

	profilesFileMu.Lock()
	defer profilesFileMu.Unlock()

	path := profilesFilePath(p.statePath)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	file := profilesFileFormat{
		Version:  profilesFileVersion,
		Profiles: p.config.LlamaCppModelProfiles,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal profiles: %w", err)
	}

	// Atomic write: tmp + rename
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write tmp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		// Cleanup tmp file
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}

	logger.Get().Debugw("SaveProfilesToFile: saved profiles to disk",
		"path", path, "count", len(file.Profiles))
	return nil
}
