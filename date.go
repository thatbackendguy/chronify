package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type dateInfo struct {
	Known      bool
	Year       int
	Month      time.Month
	Day        int
	Source     string
	Confidence string
	Timestamp  string
}

type filenameDatePattern struct {
	re         *regexp.Regexp
	yearIndex  int
	monthIndex int
	dayIndex   int
	hasDay     bool
	source     string
}

var fallbackDatePattern = regexp.MustCompile(`(\d{4})[:\-/._](\d{1,2})[:\-/._](\d{1,2})`)

var filenameDatePatterns = []filenameDatePattern{
	{
		re:         regexp.MustCompile(`(?i)(^|[^0-9])(\d{4})(\d{2})(\d{2})(?:[T_\-. ]?\d{6}(?:\d{1,6})?)?([^0-9]|$)`),
		yearIndex:  2,
		monthIndex: 3,
		dayIndex:   4,
		hasDay:     true,
		source:     "filename-yyyymmdd",
	},
	{
		re:         regexp.MustCompile(`(?i)(^|[^0-9])(\d{4})[-_. ](\d{2})[-_. ](\d{2})([^0-9]|$)`),
		yearIndex:  2,
		monthIndex: 3,
		dayIndex:   4,
		hasDay:     true,
		source:     "filename-yyyy-mm-dd",
	},
	{
		re:         regexp.MustCompile(`(?i)(^|[^0-9])(\d{4})[-_. ](\d{2})([^0-9]|$)`),
		yearIndex:  2,
		monthIndex: 3,
		hasDay:     false,
		source:     "filename-yyyy-mm",
	},
}

type metadataTools struct {
	Exiftool string
	FFprobe  string
	MDLS     string
}

func discoverMetadataTools(cfg config) metadataTools {
	if cfg.MetadataMode != "auto" {
		return metadataTools{}
	}

	return metadataTools{
		Exiftool: lookPath("exiftool"),
		FFprobe:  lookPath("ffprobe"),
		MDLS:     lookPath("mdls"),
	}
}

func lookPath(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

func determineDate(file mediaFile, cfg config, tools metadataTools) dateInfo {
	if cfg.MetadataMode != "never" {
		if info, ok := nativeMetadataDate(file.Path, cfg); ok {
			return info
		}

		if cfg.MetadataMode == "auto" {
			if info, ok := externalMetadataDate(file, cfg, tools); ok {
				return info
			}
		}
	}

	if info, ok := filenameDate(file.Name, cfg); ok {
		return info
	}

	if cfg.UseModTime {
		if info, ok := dateInfoFromTime(file.ModTime, "filesystem-modtime", "fallback", cfg); ok {
			return info
		}
	}

	return dateInfo{Known: false, Source: "unknown", Confidence: "none"}
}

// nativeReaders read capture dates directly from file contents, with no
// external tools. Each returns candidate date strings, best first.
var nativeReaders = map[string]struct {
	read   func(path string) []string
	source string
}{
	".jpg":  {jpegMetadataDates, "embedded-metadata"},
	".jpeg": {jpegMetadataDates, "embedded-metadata"},
	".png":  {pngMetadataDates, "embedded-metadata"},
	".webp": {webpMetadataDates, "embedded-metadata"},
	".heic": {heifDates, "embedded-metadata"},
	".heif": {heifDates, "embedded-metadata"},
	".avif": {heifDates, "embedded-metadata"},
	".cr3":  {cr3Dates, "embedded-metadata"},
	".raf":  {rafDates, "embedded-metadata"},
	".mp4":  {quickTimeDates, "video-metadata"},
	".mov":  {quickTimeDates, "video-metadata"},
	".m4v":  {quickTimeDates, "video-metadata"},
	".3gp":  {quickTimeDates, "video-metadata"},
	".3g2":  {quickTimeDates, "video-metadata"},
	".avi":  {aviDates, "video-metadata"},
	".mkv":  {mkvDates, "video-metadata"},
	".webm": {mkvDates, "video-metadata"},
}

func init() {
	for _, ext := range []string{".tif", ".tiff", ".dng", ".cr2", ".nef", ".arw", ".rw2", ".orf", ".pef", ".srw", ".nrw", ".rwl", ".3fr", ".erf", ".kdc", ".mrw"} {
		nativeReaders[ext] = struct {
			read   func(path string) []string
			source string
		}{tiffLikeMetadataDates, "embedded-metadata"}
	}
}

func nativeMetadataDate(path string, cfg config) (dateInfo, bool) {
	reader, ok := nativeReaders[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return dateInfo{}, false
	}

	for _, candidate := range reader.read(path) {
		if t, ok := parseDateString(candidate); ok {
			if info, ok := dateInfoFromTime(t, reader.source, "high", cfg); ok {
				return info, true
			}
		}
	}
	return dateInfo{}, false
}

func externalMetadataDate(file mediaFile, cfg config, tools metadataTools) (dateInfo, bool) {
	ext := strings.ToLower(filepath.Ext(file.Path))

	if tools.FFprobe != "" && (file.MediaType == mediaVideo || ext == ".heic" || ext == ".heif" || ext == ".avif") {
		if info, ok := ffprobeDate(file.Path, tools.FFprobe, cfg); ok {
			return info, true
		}
	}

	if tools.Exiftool != "" {
		if info, ok := exiftoolDate(file.Path, tools.Exiftool, cfg); ok {
			return info, true
		}
	}

	if runtime.GOOS == "darwin" && tools.MDLS != "" {
		if info, ok := mdlsDate(file.Path, tools.MDLS, cfg); ok {
			return info, true
		}
	}

	return dateInfo{}, false
}

func filenameDate(filename string, cfg config) (dateInfo, bool) {
	base := filepath.Base(filename)
	for _, pattern := range filenameDatePatterns {
		matches := pattern.re.FindStringSubmatch(base)
		if len(matches) == 0 {
			continue
		}

		year, ok := parseIntMatch(matches, pattern.yearIndex)
		if !ok {
			continue
		}
		month, ok := parseIntMatch(matches, pattern.monthIndex)
		if !ok {
			continue
		}

		day := 1
		confidence := "medium"
		if pattern.hasDay {
			parsedDay, ok := parseIntMatch(matches, pattern.dayIndex)
			if !ok {
				continue
			}
			day = parsedDay
			confidence = "high"
		}

		if info, ok := dateInfoFromParts(year, month, day, pattern.source, confidence, cfg); ok {
			return info, true
		}
	}
	return dateInfo{}, false
}

func parseIntMatch(matches []string, index int) (int, bool) {
	if index < 0 || index >= len(matches) || matches[index] == "" {
		return 0, false
	}
	value, err := strconv.Atoi(matches[index])
	return value, err == nil
}

func dateInfoFromParts(year, month, day int, source, confidence string, cfg config) (dateInfo, bool) {
	if year < cfg.MinYear || year > cfg.MaxYear {
		return dateInfo{}, false
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return dateInfo{}, false
	}

	t := time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.Local)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day {
		return dateInfo{}, false
	}

	return dateInfo{
		Known:      true,
		Year:       year,
		Month:      time.Month(month),
		Day:        day,
		Source:     source,
		Confidence: confidence,
		Timestamp:  t.Format("2006-01-02"),
	}, true
}

func dateInfoFromTime(t time.Time, source, confidence string, cfg config) (dateInfo, bool) {
	if t.IsZero() {
		return dateInfo{}, false
	}

	// Video containers (QuickTime/MP4) and mdls store capture times in UTC.
	// Bucketing by UTC would put a 23:30 New Year's Eve clip into the next
	// year, so zero-offset times are shown in the local timezone instead.
	if _, offset := t.Zone(); offset == 0 {
		t = t.In(time.Local)
	}

	year, month, day := t.Date()
	if year < cfg.MinYear || year > cfg.MaxYear {
		return dateInfo{}, false
	}

	return dateInfo{
		Known:      true,
		Year:       year,
		Month:      month,
		Day:        day,
		Source:     source,
		Confidence: confidence,
		Timestamp:  t.Format(time.RFC3339),
	}, true
}

func parseDateString(value string) (time.Time, bool) {
	s := strings.TrimSpace(strings.Trim(value, "\x00"))
	if s == "" || strings.Contains(s, "0000:00:00") || strings.EqualFold(s, "(null)") {
		return time.Time{}, false
	}
	if idx := strings.Index(s, "="); idx >= 0 && idx+1 < len(s) {
		s = strings.TrimSpace(s[idx+1:])
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006:01:02 15:04:05.999999-07:00",
		"2006:01:02 15:04:05.999-07:00",
		"2006:01:02 15:04:05-07:00",
		"2006:01:02 15:04:05 -07:00",
		"2006:01:02 15:04:05 -0700",
		"2006:01:02 15:04:05.999999",
		"2006:01:02 15:04:05.999",
		"2006:01:02 15:04:05",
		"2006-01-02T15:04:05-0700",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05.999",
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2006:01:02",
		"Mon Jan _2 15:04:05 2006",
		"Mon Jan 02 15:04:05 2006",
		"January 2, 2006 15:04:05",
		"January 2, 2006",
	}

	for _, layout := range layouts {
		var (
			t   time.Time
			err error
		)
		if strings.Contains(layout, "-07") || layout == time.RFC3339 || layout == time.RFC3339Nano {
			t, err = time.Parse(layout, s)
		} else {
			t, err = time.ParseInLocation(layout, s, time.Local)
		}
		if err == nil {
			return t, true
		}
	}

	matches := fallbackDatePattern.FindStringSubmatch(s)
	if len(matches) == 4 {
		year, _ := strconv.Atoi(matches[1])
		month, _ := strconv.Atoi(matches[2])
		day, _ := strconv.Atoi(matches[3])
		t := time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.Local)
		if t.Year() == year && int(t.Month()) == month && t.Day() == day {
			return t, true
		}
	}

	return time.Time{}, false
}

func ffprobeDate(path, ffprobe string, cfg config) (dateInfo, bool) {
	output, err := commandOutput(15*time.Second, ffprobe,
		"-v", "error",
		"-show_entries", "format_tags=creation_time:stream_tags=creation_time",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	if err != nil {
		return dateInfo{}, false
	}

	for _, line := range strings.Split(output, "\n") {
		if t, ok := parseDateString(line); ok {
			if info, ok := dateInfoFromTime(t, "ffprobe-creation-time", "high", cfg); ok {
				return info, true
			}
		}
	}
	return dateInfo{}, false
}

func exiftoolDate(path, exiftool string, cfg config) (dateInfo, bool) {
	output, err := commandOutput(20*time.Second, exiftool,
		"-s2",
		"-api", "QuickTimeUTC=1",
		"-DateTimeOriginal",
		"-CreateDate",
		"-MediaCreateDate",
		"-TrackCreateDate",
		"-CreationDate",
		"-ModifyDate",
		path,
	)
	if err != nil {
		return dateInfo{}, false
	}

	for _, line := range strings.Split(output, "\n") {
		tag, value := splitExiftoolLine(line)
		t, ok := parseDateString(value)
		if !ok {
			continue
		}
		// ModifyDate changes whenever a file is edited, so it is a weaker
		// signal than the capture/creation tags listed before it.
		confidence := "high"
		if tag == "ModifyDate" {
			confidence = "medium"
		}
		if info, ok := dateInfoFromTime(t, "exiftool-metadata", confidence, cfg); ok {
			return info, true
		}
	}
	return dateInfo{}, false
}

// splitExiftoolLine splits "-s2" output ("TagName: value"). Lines without a
// tag prefix are returned as a bare value.
func splitExiftoolLine(line string) (string, string) {
	line = strings.TrimSpace(line)
	idx := strings.Index(line, ": ")
	if idx <= 0 || strings.ContainsAny(line[:idx], " :") {
		return "", line
	}
	return line[:idx], strings.TrimSpace(line[idx+2:])
}

func mdlsDate(path, mdls string, cfg config) (dateInfo, bool) {
	output, err := commandOutput(10*time.Second, mdls,
		"-name", "kMDItemContentCreationDate",
		"-name", "kMDItemFSCreationDate",
		path,
	)
	if err != nil {
		return dateInfo{}, false
	}

	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}

	// Without embedded metadata, Spotlight copies the filesystem creation
	// date (when the file was copied/downloaded) into the content date. That
	// is no better than a modtime guess and must not outrank filename dates.
	content, ok := parseDateString(values["kMDItemContentCreationDate"])
	if !ok {
		return dateInfo{}, false
	}
	if fsCreated, ok := parseDateString(values["kMDItemFSCreationDate"]); ok && fsCreated.Equal(content) {
		return dateInfo{}, false
	}
	return dateInfoFromTime(content, "macos-spotlight-content-date", "medium", cfg)
}

func commandOutput(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return string(output), err
}
