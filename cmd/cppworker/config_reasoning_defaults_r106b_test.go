// config_reasoning_defaults_r106b_test.go — R106b (2026-10-12).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «не дало во вкладке настройки бекенда на странице gguf
// models поменять параметры размышления с выключенного на включенный … модель
// gemma4 при загрузке применяет не те параметры что указаны в настройках».
//
// ПРИЧИНА (часть вторая, сторона cppworker): у reasoning в конфиге ДВА поля.
//   - enableReasoning       — runtime-политика: thinking-инструкция в промпте,
//     действует сразу на все запросы;
//   - defaultEnableReasoning — значение, которое НАСЛЕДУЕТ модель при загрузке
//     (backend.go:1344: enableReasoning := cfg.DefaultEnableReasoning).
//
// Обработчик PUT /api/v1/cppworker/config/update принимал только первый: второй
// даже не значился в knownKeys. Поэтому WebUI включал размышления «для промпта»,
// а модель поднималась с reasoning_enabled=false — оператор видел в состоянии
// загруженной модели «размышления: выключено» и справедливо считал, что галочка
// не применяется.
//
// Тест держит контракт: оба ключа применяются к конфигу И сохраняются в
// config/cppworker-defaults.json (иначе после рестарта контейнера настройка
// потерялась бы).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

func TestR106b_ConfigUpdate_DefaultReasoningAppliedAndPersisted(t *testing.T) {
	// saveConfigToDefaultsFile пишет в defaultsFilePath(), то есть в каталог из
	// CPPWORKER_CONFIG_DIR. Уводим его в temp, чтобы прогон теста не затирал
	// отслеживаемый cmd/cppworker/config/cppworker-defaults.json (он копируется
	// в образ cppworker).
	//
	// Каталог config НЕ создаём намеренно: в контейнере cppworker его тоже нет
	// (рабочая директория /app), и раньше каждое сохранение из WebUI отвечало 200,
	// а на диск ничего не попадало — «no such file or directory» в логе. Тест
	// держит фикс: обработчик обязан создать каталог сам.
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", filepath.Join(dir, "config"))

	prevConfig := currentConfig
	defer func() { currentConfig = prevConfig }()
	currentConfig = &cppbackend.Config{
		Host:           "127.0.0.1",
		Port:           18091,
		ModelsDir:      dir,
		DefaultCtxSize: 32768,
	}

	body := `{"enableReasoning":true,"reasoningBudget":1024,` +
		`"defaultEnableReasoning":true,"defaultReasoningBudget":4096}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cppworker/config/update", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("config/update: HTTP %d (%s)", w.Code, w.Body.String())
	}

	if !currentConfig.EnableReasoning {
		t.Error("enableReasoning не применён (runtime-политика размышлений)")
	}
	if !currentConfig.DefaultEnableReasoning {
		t.Error("defaultEnableReasoning НЕ применён: модель при загрузке унаследует false, " +
			"и оператор снова увидит «галочка размышлений не применяется»")
	}
	if currentConfig.DefaultReasoningBudget != 4096 {
		t.Errorf("defaultReasoningBudget = %d, want 4096", currentConfig.DefaultReasoningBudget)
	}

	// Персист: без него настройка теряется после рестарта контейнера.
	// Читаем ровно тот путь, куда пишет production-код (defaultsFilePath):
	// CPPWORKER_CONFIG_DIR уводит запись в изолированный каталог — см.
	// config_defaults_path_r117_test.go.
	data, err := os.ReadFile(defaultsFilePath())
	if err != nil {
		t.Fatalf("cppworker-defaults.json не записан: %v", err)
	}
	var saved cppbackend.Config
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("cppworker-defaults.json не парсится: %v", err)
	}
	if !saved.DefaultEnableReasoning || saved.DefaultReasoningBudget != 4096 {
		t.Errorf("в файле дефолтов defaultEnableReasoning=%v budget=%d, want true/4096",
			saved.DefaultEnableReasoning, saved.DefaultReasoningBudget)
	}
}
