package types

import "time"

// StateFile - формат файла сохранения состояния
type StateFile struct {
	Version   int       `json:"version"`
	Updated   time.Time `json:"updated"`
	Backends  []Backend `json:"backends"`
}

// StateVersion - текущая версия формата state файла
const StateVersion = 1

// DefaultStatePath - путь к state файлу по умолчанию
const DefaultStatePath = "data/state.json"