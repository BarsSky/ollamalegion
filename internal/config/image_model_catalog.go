// image_model_catalog.go — R-Image (2026-09-27): загрузчик каталога пресетов
// image-моделей (config/image-model-catalog.json).
//
// ЗАЧЕМ: оператору нужен «проверенный» список bundle'ов (репозиторий + файлы +
// дефолты + runtime-флаги под слабые GPU), иначе подбор состава диффузионной
// модели превращается в угадывание по HF-поиску. Каталог — данные (JSON), а не
// код, поэтому обновляется без пересборки.
//
// ВАЛИДАЦИЯ: каждый пресет прогоняется через types.ValidateImageModelProfile —
// единственный источник правил (границы width/height/steps/cfg, обязательные
// роли файлов, обязательный VAE для DiT-семейств). Каталог, который не проходит
// валидацию, НЕ отдаётся в API: лучше явная ошибка на старте, чем «модель из
// каталога падает в sd-server». Тест internal/config проверяет файл каталога
// целиком, так что битый пресет ловится CI, а не оператором.
package config

import (
	"encoding/json"
	"fmt"
	"os"

	"ollama-loadbalancer/pkg/types"
)

const (
	// DefaultImageModelCatalogPath — путь каталога пресетов по умолчанию.
	DefaultImageModelCatalogPath = "config/image-model-catalog.json"

	// EnvImageModelCatalogPath — override пути каталога.
	EnvImageModelCatalogPath = "LB_IMAGE_MODEL_CATALOG_PATH"
)

// ImageModelCatalog — каталог пресетов image-моделей.
//
// JSON без комментариев (формат), поэтому пояснения «почему такие дефолты»
// живут в поле Notes каждого пресета и в этом файле, а не в самом .json.
type ImageModelCatalog struct {
	Version   int                       `json:"version"`
	UpdatedAt string                    `json:"updatedAt,omitempty"` // ISO 8601 (информационно)
	Presets   []types.ImageModelProfile `json:"presets"`
	// Notices — человекочитаемые оговорки по каталогу (gated-репозитории,
	// почему выбран именно этот квант и т.п.). Отдаются в API вместе с пресетами.
	Notices []string `json:"notices,omitempty"`
}

// ResolveImageModelCatalogPath — путь каталога: env → default.
func ResolveImageModelCatalogPath() string {
	if p := os.Getenv(EnvImageModelCatalogPath); p != "" {
		return p
	}
	return DefaultImageModelCatalogPath
}

// LoadImageModelCatalog читает и валидирует каталог пресетов.
func LoadImageModelCatalog(path string) (*ImageModelCatalog, error) {
	if path == "" {
		path = ResolveImageModelCatalogPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read image model catalog %s: %w", path, err)
	}
	var catalog ImageModelCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("parse image model catalog %s: %w", path, err)
	}
	if err := catalog.Validate(); err != nil {
		return nil, fmt.Errorf("invalid image model catalog %s: %w", path, err)
	}
	return &catalog, nil
}

// Validate — проверка всех пресетов каталога.
//
// Проверяем и имена (уникальность, пригодность как имя каталога bundle), и
// содержимое (types.ValidateImageModelProfile). Дубликат имени — это не
// «мелочь»: имя = каталог bundle, и два пресета с одним именем перезаписывали
// бы файлы друг друга.
func (c *ImageModelCatalog) Validate() error {
	if c == nil {
		return fmt.Errorf("catalog is nil")
	}
	seen := make(map[string]bool, len(c.Presets))
	for i := range c.Presets {
		p := &c.Presets[i]
		if err := validateImageProfileName(p.Name); err != nil {
			return fmt.Errorf("presets[%d]: %w", i, err)
		}
		if seen[p.Name] {
			return fmt.Errorf("presets[%d]: duplicate preset name %q", i, p.Name)
		}
		seen[p.Name] = true
		if err := types.ValidateImageModelProfile(p); err != nil {
			return fmt.Errorf("presets[%d] (%s): %w", i, p.Name, err)
		}
	}
	return nil
}

// Get — пресет по имени.
func (c *ImageModelCatalog) Get(name string) (types.ImageModelProfile, bool) {
	if c == nil {
		return types.ImageModelProfile{}, false
	}
	for _, p := range c.Presets {
		if p.Name == name {
			return p, true
		}
	}
	return types.ImageModelProfile{}, false
}

// Names — имена пресетов в порядке каталога (порядок = рекомендация автора).
func (c *ImageModelCatalog) Names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.Presets))
	for _, p := range c.Presets {
		names = append(names, p.Name)
	}
	return names
}

// Clone — глубокая копия пресета (API не должен отдавать указатели на кэш).
func CloneImageModelProfile(p types.ImageModelProfile) types.ImageModelProfile {
	out := p
	if p.Files != nil {
		out.Files = make([]types.ImageModelFile, len(p.Files))
		copy(out.Files, p.Files)
	}
	if p.Runtime.ExtraArgs != nil {
		out.Runtime.ExtraArgs = make([]string, len(p.Runtime.ExtraArgs))
		copy(out.Runtime.ExtraArgs, p.Runtime.ExtraArgs)
	}
	return out
}
