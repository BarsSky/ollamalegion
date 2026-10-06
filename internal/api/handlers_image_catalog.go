// handlers_image_catalog.go — R85 (2026-10-06): GET /api/v1/image/models/catalog.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ РУЧКА, ЕСЛИ ЕСТЬ /api/v1/image/capabilities. Capabilities
// отвечает на вопрос «что умеет движок» (samplers/schedulers/loras/лимиты) и
// опрашивает воркеры через HTTP на каждый промах кэша. Каталог отвечает на
// вопрос «какие модели есть, чем отличаются и можно ли их поднять» — он
// собирается из УЖЕ СОБРАННЫХ снимков imageResources (тот же поллер, что кормит
// VRAM-гейт), поэтому дешёвый и всегда согласован с гейтом: модель, которую
// каталог показывает как not_loaded, гейт не пустит в генерацию без загрузки.
//
// ЭТОТ ЖЕ КАТАЛОГ ЧИТАЕТ ИНСТРУМЕНТ (generate_image / list_image_models):
// текстовой модели нужны имена, состояния, дефолты, VRAM и описания, чтобы
// выбрать модель под запрос пользователя и поднять её самой.
//
// ПРАВА: как у остальных management-путей image-плоскости
// (AuthMiddleware + RateLimitMiddleware) — см. internal/api/routes.go.
package api

import (
	"context"
	"net/http"
	"time"
)

// imageCatalogTimeout — бюджет сбора каталога.
//
// Почему с таймаутом, а не без него: на промахе кэша сборка лениво догружает
// снимки бэкендов (ensureFreshFor, 3 с на опрос) и читает профили/каталог
// пресетов. Без бюджета запрос WebUI мог бы висеть на недоступном воркере
// сколько угодно — а каталог нужен странице для отрисовки, не для истины в
// последней инстанции (состояния моделей придут со следующего тика поллера).
const imageCatalogTimeout = 15 * time.Second

// handleImageModelsCatalog — GET /api/v1/image/models/catalog.
//
// Отдаёт каталог моделей по всем живым image_cpp-бэкендам:
//   - backends[] — id, статус, состояние воркера, VRAM, список моделей;
//   - models[]   — имя, семейство, состояние, размер, оценка VRAM, дефолты,
//     strengths/notes (описания берутся из профиля оператора, затем из каталога
//     пресетов — см. internal/balancer/image_catalog.go);
//   - limits     — границы, в которых принимается запрос на генерацию.
func (s *Server) handleImageModelsCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.proxy == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proxy unavailable"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), imageCatalogTimeout)
	defer cancel()

	s.writeJSON(w, http.StatusOK, s.proxy.ImageCatalogFor(ctx))
}
