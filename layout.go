package main

import (
	"fmt"
	"path/filepath"
	"time"
)

const (
	layoutYear  = "year"
	layoutMonth = "month"
	layoutDay   = "day"
)

// monthFormats lists the -month-format values in the order shown to users.
var monthFormats = []string{"number-long", "number-short", "number", "long", "short"}

// monthLabel names a month folder, e.g. "01 - January", "01-Jan", "01",
// "January" or "Jan".
func monthLabel(month time.Month, format string) string {
	name := month.String()
	switch format {
	case "number":
		return fmt.Sprintf("%02d", int(month))
	case "long":
		return name
	case "short":
		return name[:3]
	case "number-short":
		return fmt.Sprintf("%02d-%s", int(month), name[:3])
	default:
		return fmt.Sprintf("%02d - %s", int(month), name)
	}
}

// folderParts returns the folder names, relative to the destination root, that
// a file with this date belongs in, e.g. ["2025", "01 - January"].
func folderParts(date dateInfo, cfg config) []string {
	if !date.Known {
		return []string{cfg.UnknownDir}
	}

	parts := []string{fmt.Sprintf("%04d", date.Year)}
	if cfg.By == layoutMonth || cfg.By == layoutDay {
		parts = append(parts, monthLabel(date.Month, cfg.MonthFormat))
	}
	if cfg.By == layoutDay {
		parts = append(parts, fmt.Sprintf("%02d", date.Day))
	}
	return parts
}

func destinationPath(cfg config, result processedFile) string {
	parts := append([]string{cfg.DestRoot}, folderParts(result.Date, cfg)...)
	return filepath.Join(append(parts, result.File.Name)...)
}

// layoutExample renders a sample path for help text and the wizard.
func layoutExample(by, monthFormat string) string {
	cfg := config{By: by, MonthFormat: monthFormat}
	date := dateInfo{Known: true, Year: 2025, Month: time.January, Day: 15}
	return filepath.Join(append(folderParts(date, cfg), "IMG_0001.jpg")...)
}
