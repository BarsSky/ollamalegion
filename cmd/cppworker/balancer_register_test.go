// Тесты для Go-side auto-registration в балансировщике.
//
// Особый фокус — флаг CPPWORKER_REGISTER_DISABLE, который отключает
// дублирующую регистрацию, когда её выполняет bundled shell-script
// (docker/cppworker/register-with-balancer.sh). Без этого bundled-стек
// создаёт два бэкенда с разными id, но одним физическим cppworker'ом.

package main

import (
	"os"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

func TestIsRegisterDisabled(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{"empty", "", false},
		{"true", "true", true},
		{"True (mixed case)", "True", true},
		{"TRUE", "TRUE", true},
		{"1", "1", true},
		{"yes", "yes", true},
		{"on", "on", true},
		{" false ", " false ", false},
		{"0", "0", false},
		{"no", "no", false},
		{"off", "off", false},
		{"garbage", "garbage", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CPPWORKER_REGISTER_DISABLE", tc.val)
			got := isRegisterDisabled()
			if got != tc.want {
				t.Errorf("isRegisterDisabled()=%v, want %v (val=%q)", got, tc.want, tc.val)
			}
		})
	}
}

func TestNewBalancerRegistration_DisabledByEnv(t *testing.T) {
	// Сбрасываем все остальные env, чтобы тест был детерминированным.
	t.Setenv("CPPWORKER_BALANCER_URL", "http://loadbalancer:18081")
	t.Setenv("CPPWORKER_BALANCER_TOKEN", "secret")
	t.Setenv("CPPWORKER_REGISTER_NAME", "cppworker-test")
	t.Setenv("CPPWORKER_REGISTER_DISABLE", "true")

	cfg := &cppbackend.Config{Port: 18092}
	got := newBalancerRegistration(cfg)
	if got != nil {
		t.Errorf("newBalancerRegistration()=%+v, want nil when CPPWORKER_REGISTER_DISABLE=true", got)
	}
}

func TestNewBalancerRegistration_DisabledByEnv_MultipleValues(t *testing.T) {
	cfg := &cppbackend.Config{Port: 18092}
	for _, val := range []string{"true", "1", "yes", "on", "TRUE", "Yes"} {
		t.Run("value="+val, func(t *testing.T) {
			t.Setenv("CPPWORKER_BALANCER_URL", "http://loadbalancer:18081")
			t.Setenv("CPPWORKER_REGISTER_DISABLE", val)
			got := newBalancerRegistration(cfg)
			if got != nil {
				t.Errorf("newBalancerRegistration()=%+v, want nil for CPPWORKER_REGISTER_DISABLE=%q", got, val)
			}
		})
	}
}

func TestNewBalancerRegistration_NoURL(t *testing.T) {
	// Без URL регистрация всё равно отключена, даже если DISABLE не задан.
	t.Setenv("CPPWORKER_BALANCER_URL", "")
	t.Setenv("CPPWORKER_REGISTER_DISABLE", "")

	cfg := &cppbackend.Config{Port: 18092}
	got := newBalancerRegistration(cfg)
	if got != nil {
		t.Errorf("newBalancerRegistration()=%+v, want nil when CPPWORKER_BALANCER_URL is empty", got)
	}
}

func TestNewBalancerRegistration_EnabledByDefault(t *testing.T) {
	// Без DISABLE, но с URL — должен создаться непустой объект.
	t.Setenv("CPPWORKER_BALANCER_URL", "http://loadbalancer:18081")
	t.Setenv("CPPWORKER_BALANCER_TOKEN", "secret")
	t.Setenv("CPPWORKER_REGISTER_NAME", "cppworker-test")
	t.Setenv("CPPWORKER_REGISTER_DISABLE", "")
	t.Setenv("CPPWORKER_ADVERTISE_HOST", "test-host")
	t.Setenv("CPPWORKER_ADVERTISE_PORT", "18092")
	t.Setenv("CPPWORKER_REGISTER_GPU_MODE", "gpu")
	t.Setenv("CPPWORKER_REGISTER_HEARTBEAT", "")
	t.Setenv("CPPWORKER_REGISTER_RETRY_INTERVAL", "")
	t.Setenv("CPPWORKER_REGISTER_MAX_RETRIES", "")

	cfg := &cppbackend.Config{Port: 18092}
	got := newBalancerRegistration(cfg)
	if got == nil {
		t.Fatalf("newBalancerRegistration()=nil, want non-nil when DISABLE is not set")
	}
	if got.balancerURL != "http://loadbalancer:18081" {
		t.Errorf("balancerURL=%q, want %q", got.balancerURL, "http://loadbalancer:18081")
	}
	if got.backendID != "cppworker-test" {
		t.Errorf("backendID=%q, want %q", got.backendID, "cppworker-test")
	}
	if got.advertiseHost != "test-host" {
		t.Errorf("advertiseHost=%q, want %q", got.advertiseHost, "test-host")
	}
	if got.advertisePort != 18092 {
		t.Errorf("advertisePort=%d, want 18092", got.advertisePort)
	}
	if got.gpuMode != "gpu" {
		t.Errorf("gpuMode=%q, want %q", got.gpuMode, "gpu")
	}
}

// Sanity: убеждаемся, что в среде тестов os.Getenv для наших ключей
// работает корректно (t.Setenv должен изолировать от других тестов).
func TestEnvIsolation(t *testing.T) {
	t.Setenv("CPPWORKER_REGISTER_DISABLE", "true")
	if os.Getenv("CPPWORKER_REGISTER_DISABLE") != "true" {
		t.Fatalf("t.Setenv didn't apply: %q", os.Getenv("CPPWORKER_REGISTER_DISABLE"))
	}
}