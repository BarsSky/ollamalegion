package api

// R423 (2026-11-06): доступ API к карте per-model профилей.
//
// Карта живёт в config.LlamaCppModelProfiles и разделяется между HTTP-хендлером
// админки и обработчиками пользовательских запросов балансера: админка пишет
// профиль (PUT/DELETE /api/v1/cppworker/model-profiles, PUT AutoTune per-model,
// импорт config.json), балансер читает его в горячем пути. Прямая работа с
// картой из двух горутин — это data race, а в худшем случае фатальный
// "concurrent map read and map write".
//
// Поэтому API ходит в карту ТОЛЬКО через методы прокси (они держат profilesMu,
// см. internal/balancer/model_profiles_guard_r423.go). Ветка «без прокси» — для
// юнит-тестов, где Server собран без балансера и горутин нет.

import "ollama-loadbalancer/pkg/types"

// modelProfilesSnapshotR423 — снимок карты профилей для чтения.
func (s *Server) modelProfilesSnapshotR423() map[string]types.LlamaCppModelProfile {
	if s == nil {
		return nil
	}
	if s.proxy != nil {
		return s.proxy.ModelProfilesSnapshot()
	}
	if s.config == nil || len(s.config.LlamaCppModelProfiles) == 0 {
		return nil
	}
	out := make(map[string]types.LlamaCppModelProfile, len(s.config.LlamaCppModelProfiles))
	for name, profile := range s.config.LlamaCppModelProfiles {
		out[name] = profile
	}
	return out
}

// updateModelProfileR423 — атомарное «прочитать-изменить-записать» одной записи
// профиля. Ключ нормализуется прокси (как в SetModelProfile: имя файла
// `m.gguf` и имя модели `m` — одна запись).
func (s *Server) updateModelProfileR423(modelName string, fn func(*types.LlamaCppModelProfile)) {
	if s == nil || fn == nil {
		return
	}
	if s.proxy != nil {
		s.proxy.UpdateModelProfile(modelName, fn)
		return
	}
	if s.config == nil {
		return
	}
	if s.config.LlamaCppModelProfiles == nil {
		s.config.LlamaCppModelProfiles = make(map[string]types.LlamaCppModelProfile)
	}
	profile := s.config.LlamaCppModelProfiles[modelName]
	fn(&profile)
	s.config.LlamaCppModelProfiles[modelName] = profile
}
