package settings

// hot_settings_steward_test.go — R493-12r-fix (2026-11-13): страж «горячих»
// настроек.
//
// ЖИВОЙ ДЕФЕКТ, который этот страж ловит. Флаг `LB_STREAM_PIPE` (этап 6) был
// вписан в compose, в `.env.example` и в документацию, читался через
// `settings.HotInt`, но в РЕЕСТР не зарегистрирован. Следствия:
//
//   - `HotInt` возвращал (0,false), то есть флаг был мёртв — включить его было
//     нечем (ни WebUI, ни env: env читается только для ЗАРЕГИСТРИРОВАННЫХ
//     настроек);
//   - тесты «при флаге 0 и при флаге 1» были зелёными ОБА, потому что оба раза
//     флаг был 0 — ложное доказательство, которое я и получил;
//   - существующие стражи (`TestR274_EnvPassthrough`,
//     `TestR387_STEWARD_EverySwitchIsDocumented`) проверяют только «доезжает до
//     контейнера» и «описан оператору» — про реестр не знает ни один.
//
// Тем же стражем нашлась старая настройка `LB_BACKEND_WAIT_SEC`: код её читал,
// панель не показывала, правка из WebUI не применялась (работал только env).
//
// ПРАВИЛО. Любое имя, которое код читает через `settings.HotInt("NAME")`, обязано
// быть в реестре встроенных настроек. Исключения не заводим: если имя читается —
// оно горячее по определению, иначе его должен читать `os.Getenv`.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hotIntCallRe — вызов settings.HotInt("NAME") в исходниках.
var hotIntCallRe = regexp.MustCompile(`settings\.HotInt\("([A-Z0-9_]+)"\)`)

func TestHotSettings_AllReadNamesAreRegistered(t *testing.T) {
	roots := []string{
		filepath.Join("..", "..", "internal"),
		filepath.Join("..", "..", "cmd"),
	}
	names := map[string]string{} // имя → первый файл, где встретилось
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, m := range hotIntCallRe.FindAllStringSubmatch(string(src), -1) {
				if _, seen := names[m[1]]; !seen {
					names[m[1]] = path
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("обход %s: %v", root, err)
		}
	}
	if len(names) == 0 {
		t.Fatal("ни одного чтения settings.HotInt не найдено — страж сломан, а не код")
	}

	for name, where := range names {
		if _, ok := builtinsR278(name); !ok {
			t.Errorf("%s читает settings.HotInt(%q), но настройка НЕ зарегистрирована в "+
				"builtin_r278.go: панель её не покажет, HotInt вернёт (0,false) и флаг будет "+
				"мёртвым (ни WebUI, ни env). Зарегистрируйте её или читайте os.Getenv — "+
				"«прочитал, но не зарегистрировал» и есть ложное доказательство",
				where, name)
		}
	}
}

// TestHotSettings_StreamPipeIsRegistered — прямой тест на живой дефект: этап 6
// получил флаг, который не работал.
func TestHotSettings_StreamPipeIsRegistered(t *testing.T) {
	s, ok := builtinsR278("LB_STREAM_PIPE")
	if !ok {
		t.Fatal("LB_STREAM_PIPE не зарегистрирован: флаг этапа 6 был мёртвым — " +
			"HotInt возвращал 0, а тесты «при 0 и при 1» проходили оба раза при 0")
	}
	if s.Default != 0 {
		t.Fatalf("умолчание LB_STREAM_PIPE = %d, ожидалось 0 (историческое поведение)", s.Default)
	}
	if !s.HotReload {
		t.Fatal("LB_STREAM_PIPE обязан быть горячим: иначе переключение требует рестарта")
	}
	if strings.TrimSpace(s.Description) == "" {
		t.Fatal("без описания панель покажет пустую строку — оператор не поймёт, что включает")
	}
}
