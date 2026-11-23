// debug_bundle.go — R715 (2026-11-24): диагностический архив image-воркера.
//
// ЗАЧЕМ. Дефект D3 плана docs/plans/2026-11-24-remaining-defects.md: в архиве
// стенда (GET /api/v1/diag/bundle) для image_cpp лежал ТОЛЬКО note.txt
// «бэкенд не cppworker: архив недоступен». Вопрос «почему не грузится
// image-модель» по такому архиву не разбирался вообще: ни логов воркера, ни
// списка моделей и их состояния, ни версии образа, ни состояния загрузок.
//
// ЧТО ЗДЕСЬ. Аналог cmd/cppworker/debug_bundle.go:
//
//	GET /api/v1/sdworker/debug/bundle  (алиас /api/debug/bundle)
//
// отдаёт zip одним файлом:
//
//	info.json          — /info на момент снятия (сборка, ревизия контракта, каталоги)
//	models.json        — модели реестра и их состояние (загружена/нет/ошибка),
//	                     текущая модель, предупреждения реестра, ревизия движка
//	downloads.json     — состояние HF-загрузок (активные, история, bundle, orphans)
//	load-progress.json — последний прогресс загрузки модели (стадии, время, ошибка)
//	config.json        — действующий конфиг воркера БЕЗ секретов (HF-токена нет)
//	env.json           — только флаги диагностики/логов/каналов (белый список)
//	identity.json      — запечённая идентичность образа (R486), если она есть
//	logs.txt           — кольцевой буфер логов процесса (SDWORKER_LOG_BUFFER_ENTRIES),
//	                     повторы схлопнуты (repeated=N), хвост ограничен 2000
//	                     строками / 2 МБ с явной пометкой усечения
//	README.txt         — что это и как читать
//
// СЕКРЕТЫ. HF-токен (SDWORKER_HF_TOKEN / HF_TOKEN) в архив НЕ попадает:
// config.json собирается по списку полей, env.json — по белому списку.
package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/version"
)

const (
	// debugBundleMaxFileBytesR715 — предел на один файл архива. Архив
	// скачивают через WebUI, поэтому «побольше» — плохая идея: лишнее усекается
	// с явной пометкой.
	debugBundleMaxFileBytesR715 = 4 << 20
	// debugBundleMaxLogBytesR715 / debugBundleMaxLogLinesR715 — пределы логов
	// ИМЕННО для архива (буфер может держать больше). 2 МБ ≈ 8–10 тысяч
	// обычных строк; 2000 строк — это то, что реально читают при разборе.
	debugBundleMaxLogBytesR715 = 2 << 20
	debugBundleMaxLogLinesR715 = 2000
	// imageworkerLogBufferDefaultR715 — размер кольцевого буфера логов по
	// умолчанию. Как у cppworker (R476): без буфера архив содержал бы только
	// stdout, доступный через docker logs.
	imageworkerLogBufferDefaultR715 = 2000
	// imageworkerLogBufferMaxR715 — верхняя граница (опечатка в env не должна
	// съедать память).
	imageworkerLogBufferMaxR715 = 20000
)

// initDebugLogBufferR715 — включить кольцевой буфер логов процесса.
//
// До R715 у image-воркера брокера не было вовсе: logger.Init писал только в
// stdout, а его хвост виден лишь тому, у кого есть docker на стенде. Размер —
// SDWORKER_LOG_BUFFER_ENTRIES (дефолт 2000, 0/мусор = дефолт, отрицательное
// выключает), псевдоним SDWORKER_DIAG_RING читается только если основная не
// задана.
//
// Брокер — АГРЕГИРУЮЩИЙ (R715, как у балансера): повторяющиеся события
// занимают одну запись с repeated/last_time. Для image-воркера это особенно
// важно: «no healthy backend»/«модель не загружена» повторяются на каждый
// запрос и вытесняли бы из буфера всё остальное.
func initDebugLogBufferR715() {
	entries := imageworkerLogBufferDefaultR715
	parse := func(raw string) (int, bool) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return 0, false
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	if n, ok := parse(os.Getenv("SDWORKER_LOG_BUFFER_ENTRIES")); ok {
		entries = n
	} else if n, ok := parse(os.Getenv("SDWORKER_DIAG_RING")); ok {
		entries = n
	}
	if entries <= 0 {
		logger.Get().Infow("log buffer disabled", "entries", entries)
		return
	}
	if entries > imageworkerLogBufferMaxR715 {
		entries = imageworkerLogBufferMaxR715
	}
	logger.SetBrokerSource("imageworker")
	logger.SetBroker(logger.NewAggregatingLogBroker(entries))
	logger.Get().Infow("log buffer enabled", "entries", entries, "aggregating", true)
}

// handleDebugBundle — GET /api/v1/sdworker/debug/bundle (алиас /api/debug/bundle).
func (a *App) handleDebugBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed, use GET")
		return
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	host, _ := os.Hostname()
	if strings.TrimSpace(host) == "" {
		host = "imageworker"
	}
	name := fmt.Sprintf("imageworker-diag-%s-%s.zip", host, stamp)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)

	// `?since=30m` (или RFC3339) — только записи после этого момента: архив по
	// живому дефекту не должен тащить сутки логов.
	cutoff := debugBundleSinceCutoffR715(r.URL.Query().Get("since"))

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	addText := func(file, text string) {
		entry, err := zw.Create(file)
		if err != nil {
			logger.Get().Warnw("debug/bundle: файл не создан", "file", file, "error", err)
			return
		}
		body := text
		if len(body) > debugBundleMaxFileBytesR715 {
			body = body[:debugBundleMaxFileBytesR715] +
				"\n\n# ОБРЕЗАНО: файл больше " + fmt.Sprint(debugBundleMaxFileBytesR715) + " байт\n"
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			logger.Get().Warnw("debug/bundle: файл не записан", "file", file, "error", err)
		}
	}
	addJSON := func(file string, value interface{}) {
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			addText(file+".error.txt", err.Error()+"\n")
			return
		}
		addText(file, string(raw)+"\n")
	}

	addJSON("info.json", a.infoSnapshotR715(r.Context()))
	addJSON("models.json", a.modelsSnapshotR715())
	addJSON("downloads.json", a.downloadsSnapshotR715())
	addJSON("load-progress.json", a.progress.snapshot())
	addJSON("config.json", a.configSnapshotR715())
	addJSON("env.json", debugBundleEnvSnapshotR715())
	if id := version.BakedIdentityR486(); id != nil {
		addJSON("identity.json", id)
	}
	addText("logs.txt", debugBundleLogsTextR715(cutoff))
	addText("README.txt", debugBundleReadmeR715)

	logger.Get().Infow("debug/bundle: архив отдан", "file", name, "remote", r.RemoteAddr)
}

// infoSnapshotR715 — то же, что отдаёт GET /info (один источник с архивом:
// иначе два дампа со временем расходятся в полях).
//
// ctx — для запроса VRAM: в /info он читался на месте, и убирать поле нельзя
// (обратная совместимость ответа), а без ctx его пришлось бы выдумывать.
func (a *App) infoSnapshotR715(ctx context.Context) map[string]any {
	cfg := a.svc.Config
	return map[string]any{
		"version":                   version.Get().String(),
		"pinned_sd_server_revision": sdbackend.PinnedRevision(),
		"port":                      cfg.Port,
		"sd_server_bin":             cfg.SDServerBin,
		"sd_server_port":            cfg.ServerPort,
		"models_dir":                cfg.ModelsDir,
		"images_dir":                a.svc.Store.Dir(),
		"idle_unload_minutes":       cfg.IdleUnloadMinutes,
		"max_concurrent":            cfg.MaxConcurrent,
		"generation_timeout_sec":    cfg.GenerationTimeoutSec,
		"startup_timeout_sec":       cfg.StartupTimeoutSec,
		"base_url":                  cfg.BaseURL,
		"vram":                      sdbackend.QueryVRAM(ctx),
		"uptime_seconds":            int64(time.Since(a.startedAt).Seconds()),
		"state":                     a.svc.Sup.State(),
		"model":                     a.svc.Sup.CurrentModel(),
		"sd_server_pid":             a.svc.Sup.PID(),
		"engine_error":              a.svc.Sup.LastError(),
	}
}

// modelsSnapshotR715 — модели и их состояние.
//
// Именно этого не хватало в архиве: при «не грузится image-модель» первым делом
// нужно видеть, какие bundle вообще найдены, какая модель текущая, в каком она
// состоянии и какие у реестра предупреждения.
func (a *App) modelsSnapshotR715() map[string]any {
	out := map[string]any{
		"models":          a.svc.Models(),
		"state":           a.svc.Sup.State(),
		"current_model":   a.svc.Sup.CurrentModel(),
		"warnings":        a.svc.Registry.Warnings(),
		"pinned_revision": sdbackend.PinnedRevision(),
		"models_dir":      a.svc.Registry.ModelsDir(),
	}
	if names := a.svc.Registry.Names(); len(names) == 0 {
		out["note"] = "реестр пуст: в каталоге моделей нет ни одного bundle с profile.json"
	}
	return out
}

// downloadsSnapshotR715 — состояние HF-загрузок.
func (a *App) downloadsSnapshotR715() map[string]any {
	if a.svc.HF == nil {
		return map[string]any{
			"available": false,
			"note":      "HF-загрузчик не сконфигурирован (сервис собран без HFManager)",
		}
	}
	snap := a.svc.HF.ListDownloads()
	return map[string]any{
		"available":       true,
		"active":          snap.Active,
		"history":         snap.History,
		"bundles":         snap.Bundles,
		"bundle_history":  snap.BundleHistory,
		"orphans":         snap.Orphans,
		"active_download": a.svc.HF.HasActiveDownloads(),
	}
}

// configSnapshotR715 — действующий конфиг БЕЗ секретов.
//
// Список полей, а не сериализация sdbackend.Config целиком: в Config есть
// HFToken, и однажды добавленное поле с секретом не должно автоматически
// уезжать в архив.
func (a *App) configSnapshotR715() map[string]any {
	cfg := a.svc.Config
	return map[string]any{
		"host":                   cfg.Host,
		"port":                   cfg.Port,
		"models_dir":             cfg.ModelsDir,
		"downloads_dir":          cfg.DownloadsDir,
		"sd_server_bin":          cfg.SDServerBin,
		"sd_server_port":         cfg.ServerPort,
		"listen_ip":              cfg.ListenIP,
		"lora_model_dir":         cfg.LoraModelDir,
		"hires_upscalers_dir":    cfg.HiresUpscalersDir,
		"startup_timeout_sec":    cfg.StartupTimeoutSec,
		"generation_timeout_sec": cfg.GenerationTimeoutSec,
		"idle_unload_minutes":    cfg.IdleUnloadMinutes,
		"max_concurrent":         cfg.MaxConcurrent,
		"preload_model":          cfg.PreloadModel,
		"base_url":               cfg.BaseURL,
		"images_dir":             cfg.ImagesDir,
		"cors_origin":            cfg.CORSOrigin,
		"hf_mirror":              cfg.HFMirror,
		"hf_bundle_auto_fit":     cfg.HFBundleAutoFit,
		"hf_token_set":           strings.TrimSpace(cfg.HFToken) != "",
		"note":                   "hf_token_set — только факт наличия токена; сам токен в архив не попадает",
	}
}

// debugBundleEnvWhitelistR715 — флаги, которые нужны для разбора. Белый
// список, а не всё окружение: в env есть HF-токен и токен балансера.
var debugBundleEnvWhitelistR715 = []string{
	"SDWORKER_LOG_BUFFER_ENTRIES",
	"SDWORKER_DIAG_RING",
	"SDWORKER_CONFIG",
	"SDWORKER_MODELS_DIR",
	"SDWORKER_IMAGE_MODELS_DIR",
	"SDWORKER_DOWNLOADS_DIR",
	"SDWORKER_IMAGES_DIR",
	"SDWORKER_PRELOAD_MODEL",
	"SDWORKER_IDLE_UNLOAD_MINUTES",
	"SDWORKER_MAX_CONCURRENT",
	"SDWORKER_STARTUP_TIMEOUT_SEC",
	"SDWORKER_GENERATION_TIMEOUT_SEC",
	"SDWORKER_ALLOW_PROFILE_TIMEOUTS",
	"SDWORKER_HF_BUNDLE_AUTOFIT",
	"SDWORKER_HF_MIRROR",
	"SDWORKER_LORA_DIR",
	"SDWORKER_HIRES_UPSCALERS_DIR",
	"SDWORKER_SD_SERVER_BIN",
	"SDWORKER_CORS_ORIGIN",
	"SDWORKER_ADVERTISE_HOST",
	"SDWORKER_BACKEND_ID",
	"AGENT_EMBEDDED",
	"APP_VERSION",
	"GIT_COMMIT",
	"BUILD_DATE",
}

// debugBundleEnvSnapshotR715 — значения флагов по белому списку (пустые не пишем).
func debugBundleEnvSnapshotR715() map[string]string {
	out := make(map[string]string, len(debugBundleEnvWhitelistR715))
	for _, key := range debugBundleEnvWhitelistR715 {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			out[key] = v
		}
	}
	return out
}

// debugBundleLogsTextR715 — хвост кольцевого буфера логов.
//
// ЛОГИ ПРОЦЕССА. Если буфер выключен (SDWORKER_LOG_BUFFER_ENTRIES=0), архив
// честно пишет это: stdout контейнера доступен только через docker logs, и
// подменять его пустым файлом нельзя.
//
// ПРЕДЕЛЫ (D3, требование «не раздуть архив»): не больше
// debugBundleMaxLogLinesR715 строк и debugBundleMaxLogBytesR715 байт. Режем с
// ГОЛОВЫ (старое): свежее важнее, а усечение называется в файле явно.
func debugBundleLogsTextR715(cutoff time.Time) string {
	broker := logger.GetBroker()
	if broker == nil {
		return "# кольцевой буфер логов не включён (SDWORKER_LOG_BUFFER_ENTRIES=0)\n" +
			"# stdout процесса доступен только через docker logs <контейнер>\n"
	}
	all := broker.Snapshot()
	filtered := make([]logger.LogEntry, 0, len(all))
	for _, e := range all {
		if !cutoff.IsZero() && e.LastOccurrence().Before(cutoff) {
			continue
		}
		filtered = append(filtered, e)
	}
	if len(filtered) == 0 {
		return "# буфер пуст: с момента старта не было записей уровня логирования\n"
	}

	truncated := false
	if len(filtered) > debugBundleMaxLogLinesR715 {
		truncated = true
		filtered = filtered[len(filtered)-debugBundleMaxLogLinesR715:]
	}
	var b strings.Builder
	written := 0
	for _, e := range filtered {
		line := debugBundleLogLineR715(e)
		if b.Len()+len(line) > debugBundleMaxLogBytesR715 {
			truncated = true
			break
		}
		b.WriteString(line)
		written++
	}
	if truncated {
		fmt.Fprintf(&b, "\n# ОБРЕЗАНО (R715): показаны последние %d строк из %d "+
			"(пределы архива: %d строк / %d байт). Полный буфер — GET /api/v1/logs/export "+
			"на воркере или docker logs\n",
			written, len(all), debugBundleMaxLogLinesR715, debugBundleMaxLogBytesR715)
	}
	stats := broker.Stats()
	if !stats.FirstTime.IsZero() {
		fmt.Fprintf(&b, "# окно буфера: %s .. %s (%ds), записей в буфере %d, "+
			"пришло %d, схлопнуто повторами %d\n",
			stats.FirstTime.UTC().Format(time.RFC3339),
			stats.LastTime.UTC().Format(time.RFC3339),
			int64(stats.LastTime.Sub(stats.FirstTime).Seconds()),
			stats.Entries, stats.PublishedTotal, stats.CollapsedTotal)
	}
	return b.String()
}

// debugBundleLogLineR715 — одна строка дампа в том же формате, что у балансера
// и cppworker: время, уровень, источник, сообщение, поля key=value.
func debugBundleLogLineR715(e logger.LogEntry) string {
	src := e.Source
	if src == "" {
		src = "imageworker"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s %-5s %-9s %s",
		e.Time.Format("2006-01-02T15:04:05.000Z07:00"),
		strings.ToUpper(e.Level), src, e.Message))
	b.WriteString(logger.FieldsText(e.Fields))
	if e.Repeated > 1 {
		fmt.Fprintf(&b, " repeated=%d first=%s last=%s",
			e.Repeated,
			e.FirstOccurrence().UTC().Format("2006-01-02T15:04:05.000Z"),
			e.LastOccurrence().UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	b.WriteString("\n")
	return b.String()
}

// debugBundleSinceCutoffR715 — разобрать `since`: длительность ("30m") или
// RFC3339. Пустое/мусор = без фильтра.
func debugBundleSinceCutoffR715(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return time.Now().Add(-d)
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	logger.Get().Warnw("debug/bundle: since не разобран, фильтр не применён", "since", raw)
	return time.Time{}
}

// debugBundleReadmeR715 — краткая инструкция к архиву.
const debugBundleReadmeR715 = `Диагностический архив imageworker (sdworker, R715).

Что внутри:
  info.json          — сборка, ревизия контракта sd.cpp, каталоги, состояние движка
  models.json        — модели и их состояние (загружена/не загружена/ошибка),
                       текущая модель, предупреждения реестра
  downloads.json     — HF-загрузки: активные, история, bundle, orphan-файлы
  load-progress.json — последний прогресс загрузки модели (стадии, время, ошибка)
  config.json        — действующий конфиг без секретов (hf_token_set — только факт)
  env.json           — флаги SDWORKER_*/AGENT_* по белому списку
  identity.json      — запечённая идентичность образа (если есть)
  logs.txt           — хвост кольцевого буфера логов (до 2000 строк / 2 МБ).
                       Повторяющиеся события схлопнуты: repeated=N, first=, last=
  README.txt         — этот файл

Порядок разбора «почему не грузится image-модель»:
  1) info.json -> engine_error / state;
  2) models.json -> есть ли модель в реестре, какое состояние, какие warnings;
  3) logs.txt -> последние стадии загрузки (строки "... load ..." и ошибки движка);
  4) downloads.json -> не идёт ли до сих пор скачивание/чего не хватает в наборе.

Чего НЕТ: HF-токена и токена балансера (собирается по белому списку).
`
