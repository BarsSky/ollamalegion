// main.go — gguf-reexport: отдельный сервис ре-экспорта GGUF.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ СЕРВИС, А НЕ РУЧКА В CPPWORKER/IMAGEWORKER. Разбор целиком в
// docs/operations/gguf-reexport.ru.md; коротко: ре-экспорт — это конвертер
// llama.cpp (python + numpy, а для полного пути ещё torch), работы на десятки
// гигабайт и минуты-часы ввода-вывода. Воркер инференса обязан оставаться тонким
// и предсказуемым: он грузит модель, а не переписывает её; его образ не должен
// расти на гигабайты python-зависимостей, а операция «переупакуй 16 ГиБ» не должна
// занимать единственный слот инференса.
//
// ЧТО ЭТОТ ПРОЦЕСС ДЕЛАЕТ: держит HTTP-контракт (capabilities/jobs), очередь
// (параллелизм 1), состояние задач на диске и запускает python-драйвер
// (`py/reexport_job.py`) как дочерний процесс. Всё, что тяжело, живёт в python;
// всё, что должно быть надёжным (состояние, отмена, валидация путей), — здесь.
//
// ИДЕНТИЧНОСТЬ. R486: образ обязан доказать, чем он собран. `MustVerifyR486`
// валит контейнер, если в образе чужой/устаревший штамп, — иначе «какой конвертер
// реально работает» выясняется по логам постфактум (это уже случалось).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/version"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "проверить /health и выйти (для docker HEALTHCHECK)")
	flag.Parse()

	// Идентичность проверяем ДО всего остального: контейнер, который не может
	// доказать свою сборку, не должен принимать задачи.
	version.MustVerifyR486("gguf-reexport")

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: конфигурация: %v\n", err)
		os.Exit(1)
	}

	if *healthcheck {
		os.Exit(probeHealth(cfg))
	}

	logger.Init(envOr("LOG_LEVEL", "info"))
	log := logger.Get()
	defer logger.Sync()

	id := version.Get()
	log.Infow("gguf-reexport starting",
		"version", id.String(), "service", "gguf-reexport",
		"models_dir", cfg.ModelsDir, "out_dir", cfg.OutDir, "jobs_dir", cfg.JobsDir,
		"python", cfg.PythonBin, "driver", cfg.DriverPath,
		"llama_commit", id.LlamaCommit, "auth", cfg.APIToken != "")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cache := newModelsCache(15 * time.Second)
	info := newInfoProbe(cfg)
	// Проба python на старте: она же наполняет доступность целей, поэтому
	// первый GET /capabilities уже отвечает правдой, а не «проверка не выполнялась».
	probeCtx, cancelProbe := context.WithTimeout(ctx, 30*time.Second)
	pyInfo := info.Get(probeCtx)
	cancelProbe()
	if pyInfo.OK {
		log.Infow("python part ready", "python", pyInfo.PythonVersion,
			"torch", firstNonEmptyStr(pyInfo.TorchVersion, "нет"),
			"converter_present", pyInfo.ConverterPresent, "patch_script", pyInfo.PatchScript)
	} else {
		// НЕ fatal: сервис обязан подняться и объяснить причину через
		// /capabilities — иначе WebUI не отличит «модуля нет» от «модуль сломан».
		log.Warnw("python part not ready", "error", pyInfo.Error)
	}

	mgr := newManager(cfg, newPythonRunner(cfg, log), log)
	if err := mgr.Load(ctx); err != nil {
		log.Fatalw("не удалось загрузить состояние задач", "error", err)
	}

	srv := newServer(cfg, mgr, info, cache, log)
	if err := runServer(ctx, srv, cfg.Addr(), log); err != nil {
		log.Errorw("http server stopped", "error", err)
		os.Exit(1)
	}
	log.Infow("gguf-reexport stopped")
}

// probeHealth — `docker HEALTHCHECK CMD /app/gguf-reexport -healthcheck`.
//
// Почему через собственный бинарь, а не wget/curl: в образе на python-base нет
// curl, а тащить его ради healthcheck — лишний слой. Здесь же используется тот
// самый порт, который сервис слушает по факту.
func probeHealth(cfg config) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + cfg.Port + "/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "healthcheck: HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
