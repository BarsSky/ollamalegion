package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestParseTensorSplitEnv — unit test для CPPWORKER_TENSOR_SPLIT parsing.
//
// Проверяет: парсинг comma-separated floats + нормализация sum=1.0
// + применение к cfg.DefaultTensorSplit.
func TestParseTensorSplitEnv(t *testing.T) {
	tests := []struct {
		name        string
		envValue    string
		expectSet   bool
		expectLen   int
		expectFirst float32
	}{
		{"empty", "", false, 0, 0},
		{"single", "1.0", true, 1, 1.0},
		{"two GPUs even", "0.5,0.5", true, 2, 0.5},
		{"two GPUs uneven", "0.7,0.3", true, 2, 0.7},
		{"four GPUs", "0.25,0.25,0.25,0.25", true, 4, 0.25},
		{"with spaces", "0.5, 0.5", true, 2, 0.5},
		{"invalid value", "abc,0.5", false, 0, 0},
		{"negative", "-0.5,1.5", false, 0, 0},
		{"out of range", "2.0,0.0", false, 0, 0},
		{"empty parts", "0.5,,0.5", true, 2, 0.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &cppbackend.Config{}
			if tt.envValue != "" {
				_ = os.Setenv("CPPWORKER_TENSOR_SPLIT", tt.envValue)
			} else {
				_ = os.Unsetenv("CPPWORKER_TENSOR_SPLIT")
			}
			defer os.Unsetenv("CPPWORKER_TENSOR_SPLIT")

			// Run same parsing logic as in main.go.
			if envVal := strings.TrimSpace(os.Getenv("CPPWORKER_TENSOR_SPLIT")); envVal != "" {
				parts := strings.Split(envVal, ",")
				ts := make([]float32, 0, len(parts))
				invalid := false
				for _, p := range parts {
					p = strings.TrimSpace(p)
					if p == "" {
						continue
					}
					v, perr := strconv.ParseFloat(p, 32)
					if perr != nil || v < 0 || v > 1 {
						invalid = true
						break
					}
					ts = append(ts, float32(v))
				}
				if !invalid && len(ts) > 0 {
					var sum float32
					for _, v := range ts {
						sum += v
					}
					if sum > 0 {
						for i := range ts {
							ts[i] /= sum
						}
					}
					cfg.DefaultTensorSplit = ts
				}
			}

			if tt.expectSet {
				assert.Len(t, cfg.DefaultTensorSplit, tt.expectLen)
				if tt.expectLen > 0 {
					assert.InDelta(t, tt.expectFirst, cfg.DefaultTensorSplit[0], 0.001)
					var sum float32
					for _, v := range cfg.DefaultTensorSplit {
						sum += v
					}
					assert.InDelta(t, float32(1.0), sum, 0.001)
				}
			} else {
				assert.Nil(t, cfg.DefaultTensorSplit)
			}
		})
	}
}

// TestParseSplitModeEnv — unit test для CPPWORKER_SPLIT_MODE parsing.
func TestParseSplitModeEnv(t *testing.T) {
	tests := []struct {
		name      string
		envValue  string
		expectSet bool
		expectVal int
	}{
		{"empty", "", false, 0},
		{"default -1", "-1", true, -1},
		{"none 0", "0", true, 0},
		{"layer 1", "1", true, 1},
		{"row 2 (deprecated)", "2", true, 2},
		{"tensor 3 (experimental)", "3", true, 3},
		{"invalid -2", "-2", false, 0},
		{"invalid 4", "4", false, 0},
		{"invalid string", "abc", false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &cppbackend.Config{}
			if tt.envValue != "" {
				_ = os.Setenv("CPPWORKER_SPLIT_MODE", tt.envValue)
			} else {
				_ = os.Unsetenv("CPPWORKER_SPLIT_MODE")
			}
			defer os.Unsetenv("CPPWORKER_SPLIT_MODE")

			if envVal := strings.TrimSpace(os.Getenv("CPPWORKER_SPLIT_MODE")); envVal != "" {
				if v, err := strconv.Atoi(envVal); err == nil && v >= -1 && v <= 3 {
					cfg.DefaultSplitMode = v
				}
			}

			if tt.expectSet {
				assert.Equal(t, tt.expectVal, cfg.DefaultSplitMode)
			} else {
				assert.Equal(t, 0, cfg.DefaultSplitMode)
			}
		})
	}
}
