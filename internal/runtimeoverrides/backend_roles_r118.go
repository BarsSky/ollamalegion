// backend_roles_r118.go — R118 (2026-10-14): персист ролей бэкендов в раскладке
// слоёв.
//
// ЗАЧЕМ. Роли («главный / донор / самодостаточный» — решение оператора) лежат в
// записи бэкенда, а запись собирается заново при саморегистрации агента. Плюс в
// bundled-развёртывании config.json смонтирован read-only, поэтому штатное
// сохранение состояния молча падает (это уже описано в коде для llamaCpp — там
// сделан ровно такой же sidecar-файл). Живая проверка показала следствие: после
// перезапуска балансера роли терялись, и главный оставался без узлов
// (rpcEnabled=false).
//
// Здесь та же схема, что у llama-cpp.json: файл в writable-томе
// `balancer_data:/app/data/runtime-overrides/`.
package runtimeoverrides

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	// BackendRolesFileName — файл ролей бэкендов: {"<backendID>": "<role>"}.
	BackendRolesFileName = "backend-roles.json"
)

// backendRolesPath — полный путь к файлу ролей.
func (s *Store) backendRolesPath() string {
	return filepath.Join(s.dataDir, DirName, BackendRolesFileName)
}

// LoadBackendRoles — роли из sidecar-файла.
//
// Контракт как у LoadLlamaCpp:
//   - (roles, true, nil)   — файл есть и разобран (возможно, пустая карта);
//   - (nil, false, nil)    — файла нет (нормальная ситуация, роли не задавались);
//   - (nil, false, err)    — ошибка ввода-вывода / парсинга.
func (s *Store) LoadBackendRoles() (map[string]string, bool, error) {
	if s == nil || !s.IsEnabled() {
		return nil, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.backendRolesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	roles := map[string]string{}
	if err := json.Unmarshal(data, &roles); err != nil {
		return nil, false, err
	}
	return roles, true, nil
}

// SaveBackendRoles — записать роли бэкендов (атомарно: tmp + rename).
func (s *Store) SaveBackendRoles(roles map[string]string) error {
	if s == nil || !s.IsEnabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Join(s.dataDir, DirName), 0o755); err != nil {
		return err
	}
	if roles == nil {
		roles = map[string]string{}
	}
	data, err := json.MarshalIndent(roles, "", "  ")
	if err != nil {
		return err
	}
	path := s.backendRolesPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
