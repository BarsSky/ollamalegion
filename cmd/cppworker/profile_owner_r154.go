package main

import (
	"path/filepath"
	"strings"
)

// profileLookupNameForPath — под каким ИМЕНЕМ искать профиль модели.
//
// R154 (2026-10-16). ЖИВОЙ ДЕФЕКТ (мастер). Оператор задал параметры профилем
// `gemma-4-E4B-it-Q4_K_M` (ctx=8192, perSeq=4096, q4_0, reasoning=on). Клиент
// (или авто-подъём) запросил модель коротким именем `gemma-4`. Воркер поднял ТОТ ЖЕ
// файл `gemma-4-E4B-it-Q4_K_M.gguf`, но профиль искал ПО ИМЕНИ ЗАПРОСА: в таблице
// есть отдельная запись `gemma-4` (ctx=32768), а pickProfileByKey выбирает самое
// длинное совпадение по префиксу. В результате инстанс поднялся с параметрами
// алиаса и env-дефолтов (ctx=65536, KV из эвристики q8_0, reasoning=false) — то есть
// применение оператора молча перестало действовать, и вернуть его можно было только
// выгрузкой (повторная загрузка канонического имени отвечала 200 already_loaded:
// модель опознаётся по ФАЙЛУ).
//
// ПРАВИЛО (приоритет оператора): параметры берутся у ВЛАДЕЛЬЦА ФАЙЛА, а не у имени
// запроса. Владелец — базовое имя файла без расширения (так модели и регистрируются
// сканом), поэтому загрузка под любым алиасом получает профиль канонической модели.
//
// Пустой путь (не разрешился) — возвращаем имя запроса: гадать не о чем.
func profileLookupNameForPath(requested, modelPath string) string {
	p := strings.TrimSpace(modelPath)
	if p == "" {
		return requested
	}
	// Путь-каталог (заканчивается разделителем) владельцем не является: filepath.Base
	// вернул бы имя самого каталога. Это поймал тест.
	if strings.HasSuffix(p, "/") || strings.HasSuffix(p, `\`) {
		return requested
	}
	base := filepath.Base(p)
	base = strings.TrimSuffix(base, filepath.Ext(base)) // .gguf и прочее
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return requested
	}
	return base
}

// canonicalModelName — имя, под которым модель надо ЗАГРУЖАТЬ и РЕГИСТРИРОВАТЬ.
//
// R157 (2026-10-16). R154 уже брал параметры у владельца файла, но РЕЕСТР моделей
// продолжал знать только алиас, и запрос по каноническому имени падал на воркере:
//
//	"chat generate failed: model gemma-4-E4B-it-Q4_K_M not found"  → балансер 502
//
// Поэтому канонизируем само имя: если запрошен алиас, грузим под владельцем файла,
// а клиенту сообщаем замену заголовками X-Model-Canonical / X-Model-Requested
// (см. handlers_model.go).
func canonicalModelName(requested, modelPath string) string {
	owner := profileLookupNameForPath(requested, modelPath)
	if owner == "" {
		return requested
	}
	return owner
}
