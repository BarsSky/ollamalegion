package cppbackend

import "testing"

// R248/B3: публикация capabilities.vision по умолчанию ВКЛЮЧЕНА (приём картинок
// подключён на обеих поверхностях, включая буферизованный streaming).
// Операторский «стоп» — CPPWORKER_VISION_MMPROJ=0/false/off.
func TestR248_VisionInferenceEnabled(t *testing.T) {
	t.Setenv("CPPWORKER_VISION_MMPROJ", "")
	if !visionInferenceEnabled() {
		t.Fatal("по умолчанию публикация capability.vision должна быть включена")
	}
	for _, v := range []string{"1", "true", "YES", "on", " On "} {
		t.Setenv("CPPWORKER_VISION_MMPROJ", v)
		if !visionInferenceEnabled() {
			t.Fatalf("%q не должно выключать публикацию capability.vision", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off", " OFF "} {
		t.Setenv("CPPWORKER_VISION_MMPROJ", v)
		if visionInferenceEnabled() {
			t.Fatalf("%q должно выключать публикацию capability.vision", v)
		}
	}
}
