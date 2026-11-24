// hf_probe.go — пред-проверка файла модели: разбор заголовка вынесен в
// internal/imagemodel, здесь остались ПРЕЖНИЕ имена пакета и HTTP-путь ProbeFile.
//
// ЗАЧЕМ ФАЙЛ ВООБЩЕ ЕСТЬ. Разбор шапки (R696: роль/семейство по содержимому) с
// 2026-11-24 живёт в `internal/imagemodel` — cgo-свободном пакете: те же правила
// нужны сервису ре-экспорта (`services/gguf-reexport`, цель image_bundle),
// который не грузит llama.cpp и обязан собираться без C-моста. Правила остались
// ОДНИ (эвристика не раздвоилась), а `internal/cppbackend` сохранил все прежние
// имена: их используют воркеры, API и тесты, и менять их ради переезда файла
// значило бы править полрепозитория без пользы.
//
// ЧТО ОСТАЛОСЬ ЗДЕСЬ: `ProbeFile` — метод HuggingFaceDownloader (сеть, Range,
// разбор Content-Range живут рядом с остальным HTTP-слоем загрузчика).
//
// Экспорт: ProbeFile, ProbeHead, ProbeLocalFile, HFProbeResult, ProbeRole*,
// ProbeFamily*, HFProbeRangeBytes, EngineAnchors, SortedPrefixes.
package cppbackend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/imagemodel"
)

// HFProbeResult — результат пред-проверки файла (псевдоним типа из общего пакета).
type HFProbeResult = imagemodel.HFProbeResult

const (
	// HFProbeRangeBytes — сколько байт заголовка читаем.
	HFProbeRangeBytes = imagemodel.HFProbeRangeBytes
	// Вердикты пред-проверки.
	HFProbeSupported   = imagemodel.HFProbeSupported
	HFProbeUnknown     = imagemodel.HFProbeUnknown
	HFProbeUnsupported = imagemodel.HFProbeUnsupported
	// Роли файлов (значения совпадают с pkg/types.ImageFileRole*).
	ProbeRoleDiffusion  = imagemodel.ProbeRoleDiffusion
	ProbeRoleVae        = imagemodel.ProbeRoleVae
	ProbeRoleLLM        = imagemodel.ProbeRoleLLM
	ProbeRoleClipL      = imagemodel.ProbeRoleClipL
	ProbeRoleClipG      = imagemodel.ProbeRoleClipG
	ProbeRoleT5xxl      = imagemodel.ProbeRoleT5xxl
	ProbeRoleClipVision = imagemodel.ProbeRoleClipVision
	// Источники семейства: tensors (факт движка) | arch (подсказка).
	ProbeFamilyFromTensors = imagemodel.ProbeFamilyFromTensors
	ProbeFamilyFromArch    = imagemodel.ProbeFamilyFromArch
)

// EngineAnchors — таблица якорей семейств (псевдоним слайса из общего пакета).
//
// Слайс только читается, поэтому псевдоним — та же память: дублировать таблицу
// значило бы завести вторую правду о якорях.
var EngineAnchors = imagemodel.EngineAnchors

// ProbeHead — разбор заголовка БЕЗ сети (экспортируется для sdworker и тестов).
func ProbeHead(head []byte, filename string) *HFProbeResult {
	return imagemodel.ProbeHead(head, filename)
}

// ProbeLocalFile — заголовок файла с локального диска.
func ProbeLocalFile(path, filename string) (*HFProbeResult, error) {
	return imagemodel.ProbeLocalFile(path, filename)
}

// SortedPrefixes — отсортированные имена-префиксы тензоров (для сообщений UI).
func SortedPrefixes(prefixes []string) []string {
	return imagemodel.SortedPrefixes(prefixes)
}

// ProbeFile читает заголовок файла в HF и определяет, как его увидит движок.
func (d *HuggingFaceDownloader) ProbeFile(ctx context.Context, modelID, filename, revision string) (*HFProbeResult, error) {
	modelID = strings.TrimSpace(modelID)
	filename = strings.TrimSpace(filename)
	if modelID == "" || filename == "" {
		return nil, fmt.Errorf("modelId and filename are required")
	}
	if revision == "" {
		revision = "main"
	}
	url := d.getResolveURL(modelID, filename, revision)
	req, err := d.newRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Range: нам нужен только заголовок. HF отдаёт 206 Partial Content; если
	// зеркало/прокси Range не поддержит — придёт 200 и мы просто оборвём чтение
	// на том же лимите (io.LimitReader), не вычитывая гигабайты.
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", HFProbeRangeBytes-1))
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch header: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("header request failed: HTTP %d", resp.StatusCode)
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, HFProbeRangeBytes))
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	res := &HFProbeResult{
		ModelID:   modelID,
		Filename:  filename,
		Revision:  revision,
		CheckedB:  len(head),
		SizeBytes: parseContentRangeTotal(resp.Header.Get("Content-Range")),
	}
	// Вердикт по шапке считает общий пакет: та же функция проверяется тестами без
	// сети (internal/imagemodel).
	return imagemodel.ProbeHeadInto(res, head, filename), nil
}

// parseContentRangeTotal достаёт полный размер файла из "bytes 0-524287/4151573280".
func parseContentRangeTotal(v string) int64 {
	if v == "" {
		return 0
	}
	slash := strings.LastIndex(v, "/")
	if slash < 0 {
		return 0
	}
	var total int64
	if _, err := fmt.Sscanf(strings.TrimSpace(v[slash+1:]), "%d", &total); err != nil {
		return 0
	}
	return total
}
