//go:build llama_stub

// webui_admin_markup_r222_test.go — R222 (2026-10-22).
//
// ЗАЧЕМ. Роль viewer (гость) ограничена на сервере (мутации → 403), но UX решает
// разметка: `data-admin-only` прячет действие. До R222 атрибут стоял всего на трёх
// элементах — гость видел кнопки «Сохранить», «Загрузить», «Удалить» и получал 403
// уже по факту нажатия.
//
// Тест ловит регрессию «новичок добавил админскую кнопку и не пометил её». Список
// ниже — статические контролы index.html; динамические (карточки моделей, профили)
// проверяются поиском атрибута в рендерерах.
package api

import (
	"os"
	"strings"
	"testing"
)

func TestR222_StaticAdminControlsAreMarked(t *testing.T) {
	raw, err := os.ReadFile("../../webui/index.html")
	if err != nil {
		t.Fatalf("не читается webui/index.html: %v", err)
	}
	text := string(raw)

	// Мутирующие/конфигурационные контролы, которые обязан видеть только админ.
	ids := []string{
		"saveSettings", "resetSettings", "exportSettingsBtn", "importSettingsBtn",
		"clearLogs", "exportLogs", "restartBalancerBtn", "retakeSetupWizardBtn",
		"addBackendBtn", "exportBackends", "imageBackendsAdd", "imageProfilesNewBtn",
		"imPolicySaveBtn", "imPolicyResetBtn", "imgBundleStart", "llamaCppResetBtn",
		"modelPullBtn", "saveBackendLimitsBtn",
	}
	for _, id := range ids {
		if !strings.Contains(text, `id="`+id+`" data-admin-only`) {
			t.Errorf("контрол %q не помечен data-admin-only: гость увидит и нажмёт его", id)
		}
	}
}

// Динамические кнопки рисуются из JS: проверяем сами шаблоны, иначе после
// перерисовки списка моделей разметка ролей снова теряется.
func TestR222_DynamicAdminControlsAreMarked(t *testing.T) {
	files := []string{
		"../../webui/js/modules/gguf-renderer-detail-render.js",
		"../../webui/js/modules/renderers.js",
		"../../webui/js/modules/image-backends-page.js",
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("не читается %s: %v", path, err)
		}
		if !strings.Contains(string(raw), "data-admin-only") {
			t.Errorf("%s: ни одна динамическая админская кнопка не помечена", path)
		}
	}

	// Конкретные действия, которые были открыты гостю.
	detail := showFile(t, "../../webui/js/modules/gguf-renderer-detail-render.js")
	for _, marker := range []string{
		`gguf-load-btn" data-admin-only`,
		`gguf-unload-btn" data-admin-only`,
		`gguf-delete-btn" data-admin-only`,
		`gguf-mp-apply" data-admin-only`,
	} {
		if !strings.Contains(detail, marker) {
			t.Errorf("нет пометки %q: гость сможет загрузить/выгрузить/удалить модель или применить параметры", marker)
		}
	}
}

func showFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не читается %s: %v", path, err)
	}
	return string(raw)
}
