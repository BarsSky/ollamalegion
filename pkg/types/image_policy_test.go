package types

import (
	"strings"
	"testing"
)

// R-Image (2026-10-02): тесты гейта VRAM и политик сосуществования.
// Контракт общий для балансера (гейт/лок) и конфига — фиксируем поведение.

func TestImageResourceSettings_Defaults(t *testing.T) {
	var s ImageResourceSettings
	if got := s.EffectiveCoexistencePolicy(); got != ImageCoexistenceExclusive {
		t.Errorf("default policy = %q, want exclusive (безопасный дефолт)", got)
	}
	if got := s.EffectiveQueueWaitTimeout(); got != DefaultImageQueueWaitTimeoutSec {
		t.Errorf("default queue wait = %d, want %d", got, DefaultImageQueueWaitTimeoutSec)
	}
	if got := s.EffectiveExclusiveLockTimeout(); got != DefaultImageExclusiveLockTimeoutSec {
		t.Errorf("default lock timeout = %d, want %d", got, DefaultImageExclusiveLockTimeoutSec)
	}
	for _, p := range []ImageCoexistencePolicy{"", ImageCoexistenceExclusive, ImageCoexistenceOffload, ImageCoexistenceDedicated} {
		if !IsValidImageCoexistencePolicy(p) {
			t.Errorf("policy %q must be valid", p)
		}
	}
	if IsValidImageCoexistencePolicy("sometimes") {
		t.Error("unknown policy must be rejected")
	}
}

func TestEvaluateImageVRAM(t *testing.T) {
	est := ImageVramEstimate{RequiredMB: 4000, Source: "profile"}

	cases := []struct {
		name      string
		est       ImageVramEstimate
		freeMB    int
		settings  ImageResourceSettings
		wantAllow bool
		wantCode  string
	}{
		{
			name: "fits", est: est, freeMB: 6000,
			wantAllow: true, wantCode: ImageGateOK,
		},
		{
			name: "does not fit", est: est, freeMB: 3000,
			wantAllow: false, wantCode: ImageGateInsufficientVRAM,
		},
		{
			name: "headroom eats the budget", est: est, freeMB: 4500,
			settings:  ImageResourceSettings{VramHeadroomMB: 1000},
			wantAllow: false, wantCode: ImageGateInsufficientVRAM,
		},
		{
			name: "gate disabled", est: est, freeMB: 100,
			settings:  ImageResourceSettings{GateDisabled: true},
			wantAllow: true, wantCode: ImageGateDisabled,
		},
		{
			name: "unknown estimate does NOT block by default",
			est:  ImageVramEstimate{Source: "unknown"}, freeMB: 8000,
			wantAllow: true, wantCode: ImageGateOK,
		},
		{
			name: "unknown estimate blocks with opt-in strictness",
			est:  ImageVramEstimate{Source: "unknown"}, freeMB: 8000,
			settings:  ImageResourceSettings{BlockOnUnknownVRAMEstimate: true},
			wantAllow: false, wantCode: ImageGateUnknownEstimate,
		},
		{
			name: "free vram unknown -> do not block", est: est, freeMB: 0,
			wantAllow: true, wantCode: ImageGateOK,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := EvaluateImageVRAM(c.est, c.freeMB, c.settings)
			if v.Allowed != c.wantAllow {
				t.Fatalf("Allowed = %v, want %v (code=%s msg=%s)", v.Allowed, c.wantAllow, v.ReasonCode, v.Message)
			}
			if v.ReasonCode != c.wantCode {
				t.Fatalf("ReasonCode = %q, want %q", v.ReasonCode, c.wantCode)
			}
			if !v.Allowed {
				if v.Message == "" {
					t.Error("отказ обязан нести message (клиент показывает текст)")
				}
				if v.ReasonCode == ImageGateInsufficientVRAM && v.Hint == "" {
					t.Error("отказ по VRAM обязан нести hint (OOM-лестница), иначе оператор не знает, что делать")
				}
			}
		})
	}
}

// TestEvaluateImageVRAM_MessageExplainsShortfall — R91: отказ по VRAM обязан
// объяснять, из чего сложилась нехватка.
//
// Живой случай: оператор видел «image model needs 520 MB VRAM (+512 MB headroom),
// only 512 MB free» и читал как «не хватает 8 МБ» — при том что блокировал резерв
// сосуществования (512 МБ), а не вес модели. В тексте должны быть оба слагаемых и
// итоговая недостача, иначе непонятно, что уменьшать.
func TestEvaluateImageVRAM_MessageExplainsShortfall(t *testing.T) {
	v := EvaluateImageVRAM(
		ImageVramEstimate{RequiredMB: 520, Source: "profile"},
		512,
		ImageResourceSettings{VramHeadroomMB: 512},
	)
	if v.Allowed {
		t.Fatal("520 + 512 > 512 — гейт обязан отказать")
	}
	if !strings.Contains(v.Message, "520") || !strings.Contains(v.Message, "512") {
		t.Errorf("в сообщении нет слагаемых: %q", v.Message)
	}
	if !strings.Contains(v.Message, "reserved") {
		t.Errorf("в сообщении резерв не назван резервом: %q", v.Message)
	}
	if !strings.Contains(v.Message, "short by 520") {
		t.Errorf("в сообщении нет итоговой недостачи: %q", v.Message)
	}
	if !strings.Contains(v.Hint, "quantization") {
		t.Errorf("hint потерял OOM-лестницу: %q", v.Hint)
	}
}

func TestImageVramEstimate_IsKnown(t *testing.T) {
	if (ImageVramEstimate{}).IsKnown() {
		t.Error("пустая оценка не должна считаться известной")
	}
	if !(ImageVramEstimate{RequiredMB: 1}).IsKnown() {
		t.Error("положительная оценка должна считаться известной")
	}
}
