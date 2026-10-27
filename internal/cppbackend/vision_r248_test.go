package cppbackend

import "testing"

// R248/B3: публиковать capabilities.vision можно только при явно включённом
// приёме картинок. Пока флаг выключен (дефолт), загруженный проектор НЕ даёт
// capability — иначе балансер пропускал бы картинки, а cppworker отвечал бы
// по одному тексту (правдоподобный, но не основанный на изображении ответ).
func TestR248_VisionInferenceEnabled(t *testing.T) {
	t.Setenv("CPPWORKER_VISION_MMPROJ", "")
	if visionInferenceEnabled() {
		t.Fatal("пустое значение не должно включать публикацию capability.vision")
	}
	for _, v := range []string{"1", "true", "YES", "on", " On "} {
		t.Setenv("CPPWORKER_VISION_MMPROJ", v)
		if !visionInferenceEnabled() {
			t.Fatalf("%q должно включать публикацию capability.vision", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off", "enable"} {
		t.Setenv("CPPWORKER_VISION_MMPROJ", v)
		if visionInferenceEnabled() {
			t.Fatalf("%q не должно включать публикацию capability.vision", v)
		}
	}
}
