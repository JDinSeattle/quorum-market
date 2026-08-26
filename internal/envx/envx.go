// Package envx reads typed configuration values from the process environment.
//
// Every service in this system is configured purely through environment
// variables so that the same binary runs unchanged under docker-compose and on
// EC2 via the Terraform user-data scripts.
package envx

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// String returns the value of key, or def when unset or empty.
func String(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// Int returns key parsed as an int, or def when unset or unparseable.
func Int(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// Float returns key parsed as a float64, or def when unset or unparseable.
func Float(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

// Bool returns key parsed as a bool, or def when unset or unparseable.
func Bool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

// Millis returns key interpreted as a whole number of milliseconds.
// A bare integer ("250") and a Go duration string ("250ms") are both accepted.
func Millis(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	v = strings.TrimSpace(v)
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Millisecond
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
}

// List splits key on commas, trimming whitespace and dropping empty entries.
// An unset or all-empty value yields a nil slice rather than a slice holding
// one empty string, which is what makes `PEER_URLS=` mean "no peers".
func List(key string) []string {
	raw := os.Getenv(key)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.TrimSuffix(p, "/"))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
