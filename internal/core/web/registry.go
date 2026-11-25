package web

import (
	"time"
)

const RegistryTokenPrefix = "plr_"

// ---------- scans ----------

type ScanSummary struct {
	ID                          string
	Image, Source, Status       string
	Critical, High, Medium, Low int
	Error                       string
	CreatedAt                   time.Time
}

type Finding struct {
	ID        string `json:"id"`
	Pkg       string `json:"pkg"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed"`
	Severity  string `json:"severity"`
	Title     string `json:"title"`
}

const scanColumns = `id, image, source, status, critical, high, medium, low, error, created_at`
