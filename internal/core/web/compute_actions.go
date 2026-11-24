package web

import (
	"strings"
)

func AtoiDefault(s string, def int) int {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return Atoi(s)
}
