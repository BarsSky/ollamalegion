package modelreplication

import (
	"ollama-loadbalancer/pkg/logger"
)

// GroupAwareSelector расширяет выбор бэкенда с учётом групп репликации.
// При выборе бэкенда для модели, принадлежащей группе, учитывает:
// 1. Загруженность инстансов в группе
// 2. Лимиты minInstances/maxInstances
// 3. Целевые бэкенды
type GroupAwareSelector struct {
	manager *ModelGroupManager
}

// NewGroupAwareSelector создаёт новый селектор с учётом групп.
func NewGroupAwareSelector(manager *ModelGroupManager) *GroupAwareSelector {
	return &GroupAwareSelector{
		manager: manager,
	}
}

// Select выбирает оптимальный бэкенд для модели с учётом группы.
// Возвращает ID бэкенда или пустую строку, если модель не в группе.
func (s *GroupAwareSelector) Select(modelName string) string {
	if !s.manager.IsEnabled() {
		return ""
	}

	// Получаем группу для модели
	cfg := s.manager.GetGroup(modelName)
	if cfg == nil {
		// Модель не в группе — возвращаем пустую строку
		return ""
	}

	// Выбираем наименее загруженный инстанс
	selected := s.manager.selectInstance(modelName)
	if selected != "" {
		logger.Get().Debugw("group-aware selector selected backend",
			"model", modelName,
			"backend", selected)
	}
	return selected
}

// IsGroupModel проверяет, принадлежит ли модель группе репликации.
func (s *GroupAwareSelector) IsGroupModel(modelName string) bool {
	if !s.manager.IsEnabled() {
		return false
	}
	return s.manager.GetGroup(modelName) != nil
}

// GetGroupCandidates возвращает список бэкендов из группы для модели.
// Если модель не в группе, возвращает nil.
func (s *GroupAwareSelector) GetGroupCandidates(modelName string) []string {
	if !s.manager.IsEnabled() {
		return nil
	}

	states := s.manager.GetInstanceStates(modelName)
	if states == nil {
		return nil
	}

	candidates := make([]string, 0, len(states))
	for _, st := range states {
		if st.Status == "loaded" {
			candidates = append(candidates, st.BackendID)
		}
	}
	return candidates
}

// GetGroupStats возвращает статистику по группе для модели.
func (s *GroupAwareSelector) GetGroupStats(modelName string) map[string]interface{} {
	if !s.manager.IsEnabled() {
		return nil
	}
	return s.manager.GetGroupStats(modelName)
}
