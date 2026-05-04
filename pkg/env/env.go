// Package env provides unified helpers for reading environment variables.
package env

import (
	"log"
	"os"
	"strconv"
	"strings"
)

// Get returns the environment variable value (trimmed) or fallback if empty.
func Get(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

// GetInt returns the environment variable parsed as int or fallback on error.
// Trims whitespace and CR/LF before parsing to handle Windows line endings.
func GetInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		v = strings.TrimSpace(v)
		if n, err := strconv.Atoi(v); err == nil {
			return n
		} else {
			log.Printf("[env] WARNING: failed to parse %s=%q, using fallback %d: %v", key, v, fallback, err)
		}
	}
	return fallback
}

// GetFloat returns the environment variable parsed as float64 or fallback on error.
// Trims whitespace and CR/LF before parsing to handle Windows line endings.
func GetFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		v = strings.TrimSpace(v)
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		} else {
			log.Printf("[env] WARNING: failed to parse %s=%q, using fallback %.2f: %v", key, v, fallback, err)
		}
	}
	return fallback
}

// GetBool returns the environment variable parsed as bool or fallback on error.
// Trims whitespace and CR/LF before parsing to handle Windows line endings.
func GetBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		v = strings.TrimSpace(v)
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		} else {
			log.Printf("[env] WARNING: failed to parse %s=%q, using fallback %v: %v", key, v, fallback, err)
		}
	}
	return fallback
}
