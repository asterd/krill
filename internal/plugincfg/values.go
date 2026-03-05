package plugincfg

import (
	"fmt"
	"strings"
)

func String(cfg map[string]interface{}, key string) string {
	if cfg == nil {
		return ""
	}
	value, _ := cfg[key].(string)
	return strings.TrimSpace(value)
}

func StringDefault(cfg map[string]interface{}, key, fallback string) string {
	if value := String(cfg, key); value != "" {
		return value
	}
	return fallback
}

func Bool(cfg map[string]interface{}, key string) bool {
	if cfg == nil {
		return false
	}
	value, ok := cfg[key]
	if !ok {
		return false
	}
	switch x := value.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "true")
	default:
		return false
	}
}

func IntDefault(cfg map[string]interface{}, key string, fallback int) int {
	if cfg == nil {
		return fallback
	}
	value, ok := cfg[key]
	if !ok {
		return fallback
	}
	switch x := value.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(x), "%d", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}
