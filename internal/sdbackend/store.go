package sdbackend

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================
// Хранилище картинок (response_format:"url")
// ============================================================
//
// ЗАЧЕМ: sd-server ВСЕГДА отдаёт только b64_json и поле response_format
// игнорирует. Часть клиентов (фрагменты экосистемы, самописные боты) читают
// только data[].url — для них мы сохраняем PNG на диск и отдаём абсолютный
// URL воркера (§12.4 п.4 плана).
//
// Имена файлов — случайные (16 hex), НЕ производные от prompt: prompt может
// содержать путь/юникод/`..`, а имя файла обязано быть безопасным.

// ImageStore — каталог раздачи картинок.
type ImageStore struct {
	dir     string
	baseURL string
}

// NewImageStore — хранилище в <dir>, ссылки вида <baseURL>/images/<file>.
//
// baseURL пустой → ссылки относительные (/images/<file>): так воркер работает
// за прокси/балансером, не зная своего внешнего адреса.
func NewImageStore(dir, baseURL string) (*ImageStore, error) {
	if strings.TrimSpace(dir) == "" {
		dir = "./data/images"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve images dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create images dir %s: %w", abs, err)
	}
	return &ImageStore{dir: abs, baseURL: strings.TrimRight(baseURL, "/")}, nil
}

// Dir — каталог хранения.
func (s *ImageStore) Dir() string { return s.dir }

// Save декодирует base64 и кладёт файл; возвращает URL для клиента.
func (s *ImageStore) Save(b64 string, outputFormat string) (string, error) {
	if strings.TrimSpace(b64) == "" {
		return "", fmt.Errorf("empty image payload from sd-server")
	}
	// Движок может отдать data URL — отрезаем префикс.
	if idx := strings.Index(b64, "base64,"); idx >= 0 && strings.HasPrefix(b64, "data:") {
		b64 = b64[idx+len("base64,"):]
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("decode image base64: %w", err)
	}
	name := fmt.Sprintf("img_%d_%s.%s", time.Now().UnixNano(), randomHex(8), extFor(outputFormat))
	path := filepath.Join(s.dir, name)
	// 0644: картинки читает веб-раздача и оператор, секретов в них нет.
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write image %s: %w", path, err)
	}
	if s.baseURL != "" {
		return s.baseURL + "/images/" + name, nil
	}
	return "/images/" + name, nil
}

// extFor — расширение по output_format движка.
func extFor(outputFormat string) string {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "jpeg", "jpg":
		return "jpg"
	case "webp":
		return "webp"
	default:
		return "png"
	}
}

// randomHex — случайный hex-суффикс (защита от коллизий имён).
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:n]
	}
	return hex.EncodeToString(b)
}
