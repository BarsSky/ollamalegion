// Package env provides unified helpers for reading environment variables.
package env

import (
	"os"
	"strconv"
)

// Get returns the environment variable value or fallback if empty.
func Get(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// GetInt returns the environment variable parsed as int or fallback on error.
func GetInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// GetFloat returns the environment variable parsed as float64 or fallback on error.
func GetFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return fallback
}

// GetBool returns the environment variable parsed as bool or fallback on error.
func GetBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}