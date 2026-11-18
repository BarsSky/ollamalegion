package cppbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"ollama-loadbalancer/pkg/logger"
	"sort"
	"strings"
	"sync"
	"time"
)

// popularQuantPriority — приоритет квантизаций для рекомендации «лучшего» файла
// и сортировки в UI. Меньшее значение = выше приоритет.
var popularQuantPriority = []string{
	"Q4_K_M", "Q5_K_M", "Q6_K", "Q8_0", "Q4_0", "Q4_K_S",
	"Q3_K_M", "Q2_K", "Q5_0", "Q5_1", "Q4_1",
	"Q3_K_S", "Q3_K_L", "Q2_K_S", "F16", "F32",
	"FP16", "FP32", "BF16",
}

// quantPriority возвращает приоритет квантизации (0 = самый популярный).
func quantPriority(quant string) int {
	q := strings.ToUpper(strings.TrimSpace(quant))
	for i, p := range popularQuantPriority {
		if strings.Contains(q, p) {
			return i
		}
	}
	// При сравнении файлов: если квантизация «unknown» или пуста — ставим в конец.
	return len(popularQuantPriority) + 1
}

// pickRecommended выбирает рекомендованный .gguf (Q4_K_M если есть, иначе по приоритету).
func pickRecommended(files []HFFileInfo) string {
	if len(files) == 0 {
		return ""
	}
	best := files[0]
	bestPrio := quantPriority(best.Quantization)
	for _, f := range files[1:] {
		prio := quantPriority(f.Quantization)
		if prio < bestPrio {
			best = f
			bestPrio = prio
		}
	}
	return best.Path
}

// sortFilesByPopularity сортирует .gguf файлы по популярности квантизации.
// Сначала Q4_K_M, потом Q5_K_M и т.д. Файлы с одинаковой квантизацией
// сортируются по размеру (сначала меньшие — обычно split-файлы одинаковой квантизации).
func sortFilesByPopularity(files []HFFileInfo) {
	sort.SliceStable(files, func(i, j int) bool {
		pi := quantPriority(files[i].Quantization)
		pj := quantPriority(files[j].Quantization)
		if pi != pj {
			return pi < pj
		}
		return files[i].SizeBytes < files[j].SizeBytes
	})
}

// SearchModels выполняет поиск моделей на HuggingFace Hub.
// Для каждого результата параллельно запрашивает список .gguf файлов,
// фильтрует репозитории без GGUF и сортирует файлы по популярности.
func (d *HuggingFaceDownloader) SearchModels(ctx context.Context, query string, limit int) ([]HFModelRepo, error) {
	return d.SearchModelsTask(ctx, query, limit, "")
}

// SearchModelsTask — R-Image (2026-09-28): SearchModels с необязательным
// фильтром HF pipeline_tag (task).
//
// ЗАЧЕМ: image-воркеру нужны диффузионные репозитории (text-to-image), а не
// текстовые. Но library=gguf из SearchModels отсекал бы ВСЕ safetensors-репо,
// поэтому при явном task фильтр library не ставится: репозиторий попадает в
// выдачу, если у него есть файлы весовых форматов
// (.gguf/.safetensors/.sft/.ckpt) — их проверяет enrichWithFiles.
//
// task == "" сохраняет ровно прежнее поведение (library=gguf) — cppworker
// продолжает получать только GGUF-модели.
func (d *HuggingFaceDownloader) SearchModelsTask(ctx context.Context, query string, limit int, task string) ([]HFModelRepo, error) {
	log := logger.Get()
	log.Infow("searching HuggingFace models", "query", query, "limit", limit, "task", task)

	if limit <= 0 || limit > 100 {
		limit = 20
	}

	// HuggingFace API для поиска: GET /api/models?search=...&task=...&sort=downloads
	apiURL := d.getAPIURL("/models")
	parsedURL, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("parse API URL: %w", err)
	}

	params := url.Values{}
	params.Set("search", query)
	params.Set("sort", "downloads")
	params.Set("direction", "-1")
	params.Set("limit", fmt.Sprintf("%d", limit))
	if strings.TrimSpace(task) != "" {
		params.Set("task", strings.TrimSpace(task))
	} else {
		// Ищем модели, которые поддерживают GGUF (прежнее поведение).
		params.Set("library", "gguf")
	}
	parsedURL.RawQuery = params.Encode()

	req, err := d.newRequest("GET", parsedURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req = req.WithContext(ctx)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Парсим ответ HF API
	var hfModels []struct {
		ID          string `json:"id"`
		LastUpdated string `json:"lastModified"`
		Downloads   int    `json:"downloads"`
		Likes       int    `json:"likes"`
		PipelineTag string `json:"pipeline_tag"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&hfModels); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Конвертируем в наш тип
	result := make([]HFModelRepo, 0, len(hfModels))
	for _, m := range hfModels {
		parts := strings.SplitN(m.ID, "/", 2)
		author := ""
		name := m.ID
		if len(parts) == 2 {
			author = parts[0]
			name = parts[1]
		}

		repo := HFModelRepo{
			ID:          m.ID,
			Name:        name,
			Author:      author,
			LastUpdated: m.LastUpdated,
			Downloads:   m.Downloads,
			Likes:       m.Likes,
			PipelineTag: m.PipelineTag,
		}
		result = append(result, repo)
	}

	// Параллельно подгружаем список .gguf файлов для каждого репозитория.
	// Используем семафор на 4 одновременных запроса + 10s таймаут на каждый.
	// Репозитории без GGUF исключаем из результата.
	filtered, err := d.enrichWithFiles(ctx, result)
	if err != nil {
		log.Warnw("enrichWithFiles returned error, returning partial results", "error", err)
		// Возвращаем что есть, даже если часть не обогатилась файлами
	}

	log.Infow("search completed", "results", len(filtered), "withFiles", countWithFiles(filtered))
	return filtered, nil
}

// enrichWithFiles параллельно запрашивает ListModelFiles для каждого репо и
// обогащает результат списком .gguf файлов. Возвращает только репо с GGUF.
func (d *HuggingFaceDownloader) enrichWithFiles(ctx context.Context, repos []HFModelRepo) ([]HFModelRepo, error) {
	log := logger.Get()

	type enriched struct {
		idx   int
		repo  HFModelRepo
		files []HFFileInfo
		err   error
	}

	const maxConcurrent = 4
	sem := make(chan struct{}, maxConcurrent)
	results := make([]enriched, len(repos))

	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		go func(idx int, r HFModelRepo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Индивидуальный таймаут для каждого запроса
			fileCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			files, err := d.ListModelFiles(fileCtx, r.ID, "main")
			results[idx] = enriched{idx: idx, repo: r, files: files, err: err}
		}(i, repo)
	}
	wg.Wait()

	// Собираем результаты, фильтруя репо без .gguf файлов
	out := make([]HFModelRepo, 0, len(repos))
	for _, e := range results {
		if e.err != nil {
			log.Debugw("failed to fetch files for repo", "repo", e.repo.ID, "error", e.err)
			// Если не удалось получить файлы — пропускаем репо
			// (нет GGUF ⇒ не показываем в UI)
			continue
		}
		if len(e.files) == 0 {
			// R-Image: файлов весов нет вообще (ни .gguf, ни .safetensors).
			log.Debugw("repo has no model weight files, skipping", "repo", e.repo.ID)
			continue
		}

		// Сортируем по популярности квантизации
		sortFilesByPopularity(e.files)

		// Суммарный размер
		var total int64
		for _, f := range e.files {
			total += f.SizeBytes
		}

		e.repo.Files = e.files
		e.repo.TotalSize = total
		e.repo.HasGGUF = true
		e.repo.Recommended = pickRecommended(e.files)
		out = append(out, e.repo)
	}

	return out, nil
}

func countWithFiles(repos []HFModelRepo) int {
	n := 0
	for _, r := range repos {
		if r.HasGGUF {
			n++
		}
	}
	return n
}

// ListModelFiles получает список файлов весов в репозитории.
//
// R-Image (2026-09-27): фильтр расширений больше не зашит как «.gguf» —
// используется ModelWeightExtensions (.gguf/.safetensors/.sft/.ckpt), потому
// что bundle диффузионной модели состоит из файлов разных форматов.
func (d *HuggingFaceDownloader) ListModelFiles(ctx context.Context, modelID, revision string) ([]HFFileInfo, error) {
	return d.ListModelFilesByFormat(ctx, modelID, revision, ModelWeightExtensions)
}

// ListModelFilesByFormat — ListModelFiles с явным списком расширений
// (например []string{".gguf"} для чисто-GGUF вкладки или []string{".safetensors"}).
func (d *HuggingFaceDownloader) ListModelFilesByFormat(ctx context.Context, modelID, revision string, extensions []string) ([]HFFileInfo, error) {
	log := logger.Get()
	log.Infow("listing model files", "modelID", modelID, "revision", revision, "extensions", extensions)

	if revision == "" {
		revision = "main"
	}
	if len(extensions) == 0 {
		extensions = ModelWeightExtensions
	}

	// Используем HF API: GET /api/models/{modelID}/tree/{revision}?recursive=true
	//
	// R-Image (2026-10-03): recursive обязателен. DiT-репозитории кладут VAE и
	// text encoder в ПОДКАТАЛОГИ (vae/, text_encoders/, split_files/), а без
	// recursive=true tree-API отдаёт только корень: файлы, без которых bundle
	// DiT-модели не собрать, в списке просто не появлялись. Подкаталоги в ответе
	// приходят как type="directory" и отбрасываются ниже.
	apiURL := d.getAPIURL(fmt.Sprintf("/models/%s/tree/%s?recursive=true", modelID, revision))

	// Ответ постраничный (HF отдаёт до 1000 записей и ссылку rel="next" в Link).
	// Без обхода страниц большие репозитории (десятки шардов diffusers) потеряли бы
	// часть файлов. Потолок страниц — защита от бесконечного цикла на битом Link.
	const maxTreePages = 10
	result := make([]HFFileInfo, 0, 64)
	next := apiURL
	for page := 0; page < maxTreePages && next != ""; page++ {
		pageFiles, nextURL, err := d.fetchTreePage(ctx, next)
		if err != nil {
			if page == 0 {
				log.Warnw("failed to fetch file list, trying alternative format", "error", err)
				return d.listModelFilesAlternative(ctx, modelID, revision, extensions)
			}
			log.Warnw("stopped paging file list", "page", page, "error", err)
			break
		}
		for _, f := range pageFiles {
			if f.Type != "file" {
				continue
			}
			format := formatForExtensions(f.Path, extensions)
			if format == "" {
				continue
			}
			result = append(result, HFFileInfo{
				Path:         f.Path,
				SizeBytes:    f.Size,
				IsGGUF:       format == "gguf",
				Format:       format,
				Quantization: GetFileTypeFromName(f.Path),
			})
		}
		next = nextURL
	}

	// Сортируем по размеру (сначала маленькие)
	sort.Slice(result, func(i, j int) bool {
		return result[i].SizeBytes < result[j].SizeBytes
	})

	log.Infow("files listed", "modelID", modelID, "modelFiles", len(result))
	return result, nil
}

// treeEntry — одна запись tree-API HuggingFace.
type treeEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Type string `json:"type"`
}

// fetchTreePage читает одну страницу tree-API и возвращает URL следующей
// страницы (пустая строка — страниц больше нет).
//
// Ссылку следующей страницы берём из заголовка Link, но собираем её САМИ:
// HF отдаёт абсолютный URL на huggingface.co, а мы обязаны остаться на
// настроенном зеркале (иначе токен и трафик ушли бы мимо него). Из ссылки
// переносим только cursor.
func (d *HuggingFaceDownloader) fetchTreePage(ctx context.Context, url string) ([]treeEntry, string, error) {
	req, err := d.newRequest("GET", url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	req = req.WithContext(ctx)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("API returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var entries []treeEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, "", fmt.Errorf("parse file list: %w", err)
	}
	return entries, nextTreePageURL(url, resp.Header.Get("Link")), nil
}

// nextTreePageURL — URL следующей страницы: та же база/путь, но cursor из Link.
func nextTreePageURL(current, link string) string {
	next := parseLinkNext(link)
	if next == "" {
		return ""
	}
	parsed, err := url.Parse(next)
	if err != nil {
		return ""
	}
	cursor := parsed.Query().Get("cursor")
	if cursor == "" {
		return ""
	}
	base, err := url.Parse(current)
	if err != nil {
		return ""
	}
	q := base.Query()
	q.Set("cursor", cursor)
	base.RawQuery = q.Encode()
	return base.String()
}

// parseLinkNext достаёт URL из заголовка Link по rel="next".
func parseLinkNext(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start >= 0 && end > start {
			return part[start+1 : end]
		}
	}
	return ""
}

// formatForExtensions — формат файла, если его расширение входит в allowed;
// иначе "". Отдельный helper (а не прямой HasModelWeightExtension), чтобы
// явный список расширений уважался в обоих путях (tree и siblings).
func formatForExtensions(path string, allowed []string) string {
	lower := strings.ToLower(path)
	for _, ext := range allowed {
		if strings.HasSuffix(lower, strings.ToLower(ext)) {
			return strings.TrimPrefix(strings.ToLower(ext), ".")
		}
	}
	return ""
}

// listModelFilesAlternative — альтернативный метод получения файлов
// Использует прямой HTML парсинг или API /api/models/{modelID}
func (d *HuggingFaceDownloader) listModelFilesAlternative(ctx context.Context, modelID, revision string, extensions []string) ([]HFFileInfo, error) {
	// Пробуем получить список через API модели
	apiURL := d.getAPIURL(fmt.Sprintf("/models/%s", modelID))

	req, err := d.newRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req = req.WithContext(ctx)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d for model info", resp.StatusCode)
	}

	// Парсим инфу о модели, ищем siblings (файлы)
	var modelInfo struct {
		Siblings []struct {
			Rfilename string `json:"rfilename"`
			Size      int64  `json:"size"`
		} `json:"siblings"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&modelInfo); err != nil {
		return nil, fmt.Errorf("decode model info: %w", err)
	}

	result := make([]HFFileInfo, 0, len(modelInfo.Siblings))
	for _, s := range modelInfo.Siblings {
		format := formatForExtensions(s.Rfilename, extensions)
		if format == "" {
			continue
		}
		result = append(result, HFFileInfo{
			Path:         s.Rfilename,
			SizeBytes:    s.Size,
			IsGGUF:       format == "gguf",
			Format:       format,
			Quantization: GetFileTypeFromName(s.Rfilename),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].SizeBytes < result[j].SizeBytes
	})

	return result, nil
}

// ============================================================
// Загрузка моделей
// ============================================================
