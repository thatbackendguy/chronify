package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	mediaImage = "image"
	mediaVideo = "video"
)

var imageExtensions = map[string]struct{}{
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".bmp": {},
	".tif": {}, ".tiff": {}, ".webp": {}, ".heic": {}, ".heif": {},
	".avif": {}, ".jxl": {},
	".raw": {}, ".dng": {}, ".cr2": {}, ".cr3": {}, ".nef": {},
	".arw": {}, ".raf": {}, ".rw2": {}, ".orf": {}, ".pef": {},
	".srw": {}, ".3fr": {}, ".erf": {}, ".kdc": {}, ".mrw": {},
	".nrw": {}, ".rwl": {},
}

var videoExtensions = map[string]struct{}{
	".mp4": {}, ".mov": {}, ".m4v": {}, ".avi": {}, ".mkv": {},
	".webm": {}, ".mpg": {}, ".mpeg": {}, ".mts": {}, ".m2ts": {},
	".3gp": {}, ".3g2": {}, ".wmv": {}, ".flv": {}, ".hevc": {},
	".mod": {}, ".tod": {}, ".vob": {},
}

type config struct {
	SourceRoot    string
	DestRoot      string
	ReportPath    string
	UnknownDir    string
	Mode          string
	Conflict      string
	MetadataMode  string
	MediaFilter   string
	Apply         bool
	UseModTime    bool
	IncludeHidden bool
	Verbose       bool
	Workers       int
	MinYear       int
	MaxYear       int
	ProgressEvery int64
}

type metadataTools struct {
	Exiftool string
	FFprobe  string
	MDLS     string
}

type mediaFile struct {
	Path      string
	RelPath   string
	Name      string
	MediaType string
	Size      int64
	ModTime   time.Time
	Mode      fs.FileMode
}

type dateInfo struct {
	Known      bool
	Year       int
	Month      time.Month
	Day        int
	Source     string
	Confidence string
	Timestamp  string
}

type processedFile struct {
	File mediaFile
	Date dateInfo
}

type actionResult struct {
	Action      string
	Status      string
	Destination string
	Error       error
}

type stats struct {
	Processed     int64
	Planned       int64
	Moved         int64
	Copied        int64
	Skipped       int64
	Failed        int64
	UnknownDates  int64
	ImageFiles    int64
	VideoFiles    int64
	BytesPlanned  int64
	BytesMoved    int64
	BytesCopied   int64
	DateSourceHit map[string]int64
	MonthHit      map[string]int64
}

type filenameDatePattern struct {
	re         *regexp.Regexp
	yearIndex  int
	monthIndex int
	dayIndex   int
	hasDay     bool
	source     string
}

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

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n\n", err)
		flag.Usage()
		os.Exit(2)
	}

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Organizer failed: %v\n", err)
		os.Exit(1)
	}
}

func parseConfig() (config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config{}, err
	}

	now := time.Now()
	cfg := config{
		SourceRoot:    cwd,
		DestRoot:      cwd,
		ReportPath:    filepath.Join(cwd, fmt.Sprintf("organizer-report-%s.csv", now.Format("20060102-150405"))),
		UnknownDir:    "_unknown_date",
		Mode:          "move",
		Conflict:      "rename",
		MetadataMode:  "auto",
		MediaFilter:   "all",
		UseModTime:    true,
		Workers:       defaultWorkers(),
		MinYear:       1900,
		MaxYear:       now.Year() + 1,
		ProgressEvery: 1000,
	}

	flag.Usage = func() {
		exe := filepath.Base(os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), `Media Organizer

Organizes photos and videos into folders like 2024/05 without loading file
contents into memory. It dry-runs by default and writes a CSV manifest.

Examples:
  %[1]s -source "/Volumes/Camera Dump" -dest "/Volumes/Media Library"
  %[1]s -source "/Volumes/Camera Dump" -dest "/Volumes/Media Library" -apply -mode move
  %[1]s -source "/Volumes/Camera Dump" -dest "/Volumes/Media Library" -apply -mode copy

Flags:
`, exe)
		flag.PrintDefaults()
	}

	flag.StringVar(&cfg.SourceRoot, "source", cfg.SourceRoot, "folder to scan recursively")
	flag.StringVar(&cfg.DestRoot, "dest", cfg.DestRoot, "folder where YYYY/MM folders will be created")
	flag.StringVar(&cfg.ReportPath, "report", cfg.ReportPath, `CSV manifest path; use "none" to disable`)
	flag.StringVar(&cfg.UnknownDir, "unknown-dir", cfg.UnknownDir, "folder for files with no usable date when -modtime=false")
	flag.StringVar(&cfg.Mode, "mode", cfg.Mode, "operation to perform when -apply is set: move or copy")
	flag.StringVar(&cfg.Conflict, "conflict", cfg.Conflict, "what to do when destination exists: rename or skip")
	flag.StringVar(&cfg.MetadataMode, "metadata", cfg.MetadataMode, "date metadata strategy: auto, native, or never")
	flag.StringVar(&cfg.MediaFilter, "media", cfg.MediaFilter, "which files to include: all, images, or videos")
	flag.BoolVar(&cfg.Apply, "apply", cfg.Apply, "actually move/copy files; omit for a dry-run")
	flag.BoolVar(&cfg.UseModTime, "modtime", cfg.UseModTime, "fall back to filesystem modified time when no capture date is found")
	flag.BoolVar(&cfg.IncludeHidden, "include-hidden", cfg.IncludeHidden, "include dot-prefixed files and folders")
	flag.BoolVar(&cfg.Verbose, "verbose", cfg.Verbose, "print every planned/applied file action")
	flag.IntVar(&cfg.Workers, "workers", cfg.Workers, "parallel date-detection workers")
	flag.IntVar(&cfg.MinYear, "min-year", cfg.MinYear, "minimum accepted media year")
	flag.IntVar(&cfg.MaxYear, "max-year", cfg.MaxYear, "maximum accepted media year")
	flag.Int64Var(&cfg.ProgressEvery, "progress-every", cfg.ProgressEvery, "print progress after this many processed files; 0 disables count-based progress")
	flag.Parse()

	if cfg.SourceRoot, err = filepath.Abs(cfg.SourceRoot); err != nil {
		return config{}, err
	}
	if cfg.DestRoot, err = filepath.Abs(cfg.DestRoot); err != nil {
		return config{}, err
	}
	if cfg.ReportPath != "" && !strings.EqualFold(cfg.ReportPath, "none") {
		if cfg.ReportPath, err = filepath.Abs(cfg.ReportPath); err != nil {
			return config{}, err
		}
	}

	if cfg.Workers < 1 {
		return config{}, fmt.Errorf("-workers must be at least 1")
	}
	if cfg.MinYear < 1 || cfg.MaxYear < cfg.MinYear {
		return config{}, fmt.Errorf("year bounds are invalid")
	}

	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch cfg.Mode {
	case "move", "copy":
	default:
		return config{}, fmt.Errorf("-mode must be move or copy")
	}

	cfg.Conflict = strings.ToLower(strings.TrimSpace(cfg.Conflict))
	switch cfg.Conflict {
	case "rename", "skip":
	default:
		return config{}, fmt.Errorf("-conflict must be rename or skip")
	}

	cfg.MetadataMode = strings.ToLower(strings.TrimSpace(cfg.MetadataMode))
	switch cfg.MetadataMode {
	case "auto", "native", "never":
	default:
		return config{}, fmt.Errorf("-metadata must be auto, native, or never")
	}

	cfg.MediaFilter = strings.ToLower(strings.TrimSpace(cfg.MediaFilter))
	switch cfg.MediaFilter {
	case "all", "images", "videos":
	default:
		return config{}, fmt.Errorf("-media must be all, images, or videos")
	}

	info, err := os.Stat(cfg.SourceRoot)
	if err != nil {
		return config{}, fmt.Errorf("source folder is not readable: %w", err)
	}
	if !info.IsDir() {
		return config{}, fmt.Errorf("source is not a folder: %s", cfg.SourceRoot)
	}

	return cfg, nil
}

func defaultWorkers() int {
	workers := runtime.NumCPU() / 2
	if workers < 2 {
		return 2
	}
	if workers > 8 {
		return 8
	}
	return workers
}

func run(cfg config) error {
	tools := discoverMetadataTools(cfg)
	reportFile, reportWriter, err := openReport(cfg.ReportPath)
	if err != nil {
		return err
	}
	if reportFile != nil {
		defer reportFile.Close()
		defer reportWriter.Flush()
		if err := writeReportHeader(reportWriter); err != nil {
			return err
		}
	}

	printRunHeader(cfg, tools)

	jobs := make(chan mediaFile, cfg.Workers*4)
	results := make(chan processedFile, cfg.Workers*4)
	scanned := int64(0)
	scanErr := make(chan error, 1)

	var wg sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range jobs {
				results <- processedFile{
					File: file,
					Date: determineDate(file, cfg, tools),
				}
			}
		}()
	}

	go func() {
		scanErr <- scanMediaFiles(cfg, jobs, &scanned)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	s := stats{
		DateSourceHit: make(map[string]int64),
		MonthHit:      make(map[string]int64),
	}

	for result := range results {
		action := executeAction(cfg, result)
		s.record(result, action)

		if reportWriter != nil {
			if err := writeReportRow(reportWriter, result, action); err != nil {
				return err
			}
		}

		if cfg.Verbose {
			printAction(result, action)
		} else if cfg.ProgressEvery > 0 && s.Processed%cfg.ProgressEvery == 0 {
			fmt.Printf("Processed %d media files (scanned %d so far)\n", s.Processed, atomic.LoadInt64(&scanned))
		}
	}

	if err := <-scanErr; err != nil {
		return err
	}

	if reportWriter != nil {
		reportWriter.Flush()
		if err := reportWriter.Error(); err != nil {
			return err
		}
	}

	printSummary(cfg, s, atomic.LoadInt64(&scanned))
	return nil
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

func printRunHeader(cfg config, tools metadataTools) {
	mode := "DRY RUN"
	if cfg.Apply {
		mode = strings.ToUpper(cfg.Mode)
	}

	fmt.Println("Media Organizer by Year/Month")
	fmt.Println(strings.Repeat("=", 34))
	fmt.Printf("Mode:        %s\n", mode)
	fmt.Printf("Source:      %s\n", cfg.SourceRoot)
	fmt.Printf("Destination: %s\n", cfg.DestRoot)
	fmt.Printf("Media:       %s\n", cfg.MediaFilter)
	fmt.Printf("Workers:     %d\n", cfg.Workers)
	fmt.Printf("Years:       %d-%d\n", cfg.MinYear, cfg.MaxYear)
	if cfg.ReportPath != "" && !strings.EqualFold(cfg.ReportPath, "none") {
		fmt.Printf("Report:      %s\n", cfg.ReportPath)
	}
	fmt.Printf("Metadata:    %s", cfg.MetadataMode)
	if cfg.MetadataMode == "auto" {
		fmt.Printf(" (exiftool=%s, ffprobe=%s, mdls=%s)", yesNo(tools.Exiftool != ""), yesNo(tools.FFprobe != ""), yesNo(tools.MDLS != ""))
	}
	fmt.Println()
	fmt.Println()
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func scanMediaFiles(cfg config, jobs chan<- mediaFile, scanned *int64) error {
	defer close(jobs)

	source := filepath.Clean(cfg.SourceRoot)
	dest := filepath.Clean(cfg.DestRoot)
	skipDest := !samePath(source, dest) && isSubpath(dest, source)

	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			fmt.Fprintf(os.Stderr, "Skipping unreadable path %s: %v\n", path, walkErr)
			return nil
		}

		if entry.IsDir() {
			if path == source {
				return nil
			}
			if skipDest && samePath(path, dest) {
				return filepath.SkipDir
			}
			if !cfg.IncludeHidden && isHidden(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		mediaType, ok := mediaTypeFor(entry.Name())
		if !ok || !mediaFilterMatches(mediaType, cfg.MediaFilter) {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Skipping unreadable file %s: %v\n", path, err)
			return nil
		}

		rel, err := filepath.Rel(source, path)
		if err != nil {
			rel = entry.Name()
		}

		atomic.AddInt64(scanned, 1)
		jobs <- mediaFile{
			Path:      path,
			RelPath:   rel,
			Name:      entry.Name(),
			MediaType: mediaType,
			Size:      info.Size(),
			ModTime:   info.ModTime(),
			Mode:      info.Mode(),
		}
		return nil
	})
}

func mediaTypeFor(filename string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(filename))
	if _, ok := imageExtensions[ext]; ok {
		return mediaImage, true
	}
	if _, ok := videoExtensions[ext]; ok {
		return mediaVideo, true
	}
	return "", false
}

func mediaFilterMatches(mediaType, filter string) bool {
	switch filter {
	case "all":
		return true
	case "images":
		return mediaType == mediaImage
	case "videos":
		return mediaType == mediaVideo
	default:
		return false
	}
}

func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
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

func nativeMetadataDate(path string, cfg config) (dateInfo, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	var candidates []string

	switch ext {
	case ".jpg", ".jpeg":
		candidates = jpegMetadataDates(path)
	case ".tif", ".tiff", ".dng", ".cr2", ".nef", ".arw", ".rw2", ".orf", ".pef", ".srw", ".nrw", ".rwl":
		candidates = tiffLikeMetadataDates(path)
	case ".png":
		candidates = pngMetadataDates(path)
	case ".webp":
		candidates = webpMetadataDates(path)
	}

	for _, candidate := range candidates {
		if t, ok := parseDateString(candidate); ok {
			if info, ok := dateInfoFromTime(t, "embedded-metadata", "high", cfg); ok {
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

	fallback := regexp.MustCompile(`(\d{4})[:\-/._](\d{1,2})[:\-/._](\d{1,2})`)
	matches := fallback.FindStringSubmatch(s)
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

func jpegMetadataDates(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	header := make([]byte, 2)
	if _, err := io.ReadFull(file, header); err != nil || header[0] != 0xff || header[1] != 0xd8 {
		return nil
	}

	for {
		markerPrefix := make([]byte, 1)
		if _, err := io.ReadFull(file, markerPrefix); err != nil {
			return nil
		}
		if markerPrefix[0] != 0xff {
			continue
		}

		marker := make([]byte, 1)
		if _, err := io.ReadFull(file, marker); err != nil {
			return nil
		}
		for marker[0] == 0xff {
			if _, err := io.ReadFull(file, marker); err != nil {
				return nil
			}
		}

		if marker[0] == 0xda || marker[0] == 0xd9 {
			return nil
		}

		lengthBytes := make([]byte, 2)
		if _, err := io.ReadFull(file, lengthBytes); err != nil {
			return nil
		}
		length := int(binary.BigEndian.Uint16(lengthBytes))
		if length < 2 {
			return nil
		}
		dataLength := length - 2

		if marker[0] != 0xe1 {
			if _, err := file.Seek(int64(dataLength), io.SeekCurrent); err != nil {
				return nil
			}
			continue
		}

		data := make([]byte, dataLength)
		if _, err := io.ReadFull(file, data); err != nil {
			return nil
		}
		if bytes.HasPrefix(data, []byte("Exif\x00\x00")) {
			return parseTIFFDates(data[6:])
		}
	}
}

func tiffLikeMetadataDates(path string) []string {
	data, err := readPrefix(path, 2*1024*1024)
	if err != nil {
		return nil
	}
	return parseTIFFDates(data)
}

func pngMetadataDates(path string) []string {
	data, err := readPrefix(path, 2*1024*1024)
	if err != nil || len(data) < 8 || !bytes.Equal(data[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return nil
	}

	var candidates []string
	offset := 8
	for offset+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		chunkType := string(data[offset+4 : offset+8])
		chunkStart := offset + 8
		chunkEnd := chunkStart + length
		if length < 0 || chunkEnd > len(data) {
			break
		}

		switch chunkType {
		case "eXIf":
			candidates = append(candidates, parseTIFFDates(data[chunkStart:chunkEnd])...)
		case "tEXt":
			chunk := data[chunkStart:chunkEnd]
			parts := bytes.SplitN(chunk, []byte{0}, 2)
			if len(parts) == 2 && looksLikeDateKey(string(parts[0])) {
				candidates = append(candidates, string(parts[1]))
			}
		case "IDAT":
			if len(candidates) > 0 {
				return candidates
			}
		}

		offset = chunkEnd + 4
	}
	return candidates
}

func webpMetadataDates(path string) []string {
	data, err := readPrefix(path, 4*1024*1024)
	if err != nil || len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil
	}

	var candidates []string
	offset := 12
	for offset+8 <= len(data) {
		chunkType := string(data[offset : offset+4])
		length := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		chunkStart := offset + 8
		chunkEnd := chunkStart + length
		if length < 0 || chunkEnd > len(data) {
			break
		}
		if chunkType == "EXIF" {
			candidates = append(candidates, parseTIFFDates(data[chunkStart:chunkEnd])...)
		}
		offset = chunkEnd
		if offset%2 == 1 {
			offset++
		}
	}
	return candidates
}

func looksLikeDateKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(key, "date") || strings.Contains(key, "time") || strings.Contains(key, "creation")
}

func readPrefix(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var buffer bytes.Buffer
	_, err = io.Copy(&buffer, io.LimitReader(file, limit))
	if err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func parseTIFFDates(data []byte) []string {
	if len(data) < 8 {
		return nil
	}

	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return nil
	}

	if order.Uint16(data[2:4]) != 42 {
		return nil
	}

	firstIFD := int(order.Uint32(data[4:8]))
	return parseIFDDates(data, firstIFD, order, 0)
}

func parseIFDDates(data []byte, offset int, order binary.ByteOrder, depth int) []string {
	if depth > 4 || offset < 0 || offset+2 > len(data) {
		return nil
	}

	count := int(order.Uint16(data[offset : offset+2]))
	entriesStart := offset + 2
	entriesEnd := entriesStart + count*12
	if count < 0 || entriesEnd > len(data) {
		return nil
	}

	var ifdDates []string
	var subIFDOffsets []int
	for i := 0; i < count; i++ {
		entry := data[entriesStart+i*12 : entriesStart+(i+1)*12]
		tag := order.Uint16(entry[0:2])
		fieldType := order.Uint16(entry[2:4])
		fieldCount := order.Uint32(entry[4:8])

		switch tag {
		case 0x0132, 0x9003, 0x9004:
			if fieldType != 2 {
				continue
			}
			if value, ok := tiffEntryBytes(data, entry, order, fieldType, fieldCount); ok {
				ifdDates = append(ifdDates, strings.TrimRight(string(value), "\x00 "))
			}
		case 0x8769, 0x8825:
			if pointer, ok := tiffEntryUint(entry, order, fieldType); ok {
				subIFDOffsets = append(subIFDOffsets, int(pointer))
			}
		}
	}

	var subDates []string
	for _, subOffset := range subIFDOffsets {
		subDates = append(subDates, parseIFDDates(data, subOffset, order, depth+1)...)
	}

	return append(subDates, ifdDates...)
}

func tiffEntryBytes(data, entry []byte, order binary.ByteOrder, fieldType uint16, fieldCount uint32) ([]byte, bool) {
	typeSize, ok := tiffTypeSize(fieldType)
	if !ok {
		return nil, false
	}

	total := uint64(typeSize) * uint64(fieldCount)
	if total == 0 || total > uint64(len(data)) {
		return nil, false
	}

	if total <= 4 {
		return entry[8 : 8+int(total)], true
	}

	valueOffset := int(order.Uint32(entry[8:12]))
	valueEnd := valueOffset + int(total)
	if valueOffset < 0 || valueEnd > len(data) {
		return nil, false
	}
	return data[valueOffset:valueEnd], true
}

func tiffEntryUint(entry []byte, order binary.ByteOrder, fieldType uint16) (uint32, bool) {
	switch fieldType {
	case 3:
		return uint32(order.Uint16(entry[8:10])), true
	case 4:
		return order.Uint32(entry[8:12]), true
	default:
		return 0, false
	}
}

func tiffTypeSize(fieldType uint16) (uint32, bool) {
	switch fieldType {
	case 1, 2, 6, 7:
		return 1, true
	case 3, 8:
		return 2, true
	case 4, 9, 11:
		return 4, true
	case 5, 10, 12:
		return 8, true
	default:
		return 0, false
	}
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
		"-s3",
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
		if t, ok := parseDateString(line); ok {
			if info, ok := dateInfoFromTime(t, "exiftool-metadata", "high", cfg); ok {
				return info, true
			}
		}
	}
	return dateInfo{}, false
}

func mdlsDate(path, mdls string, cfg config) (dateInfo, bool) {
	output, err := commandOutput(10*time.Second, mdls,
		"-raw",
		"-name", "kMDItemContentCreationDate",
		"-name", "kMDItemFSCreationDate",
		path,
	)
	if err != nil {
		return dateInfo{}, false
	}

	for _, line := range strings.Split(output, "\n") {
		if t, ok := parseDateString(line); ok {
			if info, ok := dateInfoFromTime(t, "macos-mdls-creation-date", "medium", cfg); ok {
				return info, true
			}
		}
	}
	return dateInfo{}, false
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

func executeAction(cfg config, result processedFile) actionResult {
	destination := destinationPath(cfg, result)
	if samePath(result.File.Path, destination) {
		return actionResult{
			Action:      "skip",
			Status:      "already_in_place",
			Destination: destination,
		}
	}

	finalDestination, skipped, err := resolveDestination(destination, cfg.Conflict)
	if err != nil {
		return actionResult{
			Action:      cfg.Mode,
			Status:      "failed",
			Destination: destination,
			Error:       err,
		}
	}
	if skipped {
		return actionResult{
			Action:      "skip",
			Status:      "destination_exists",
			Destination: destination,
		}
	}

	if !cfg.Apply {
		return actionResult{
			Action:      "would_" + cfg.Mode,
			Status:      "planned",
			Destination: finalDestination,
		}
	}

	if err := os.MkdirAll(filepath.Dir(finalDestination), 0755); err != nil {
		return actionResult{
			Action:      cfg.Mode,
			Status:      "failed",
			Destination: finalDestination,
			Error:       err,
		}
	}

	switch cfg.Mode {
	case "copy":
		err = copyFile(result.File.Path, finalDestination, result.File.Mode, result.File.ModTime)
	case "move":
		err = moveFile(result.File.Path, finalDestination, result.File.Mode, result.File.ModTime)
	}
	if err != nil {
		return actionResult{
			Action:      cfg.Mode,
			Status:      "failed",
			Destination: finalDestination,
			Error:       err,
		}
	}

	return actionResult{
		Action:      cfg.Mode,
		Status:      pastTense(cfg.Mode),
		Destination: finalDestination,
	}
}

func pastTense(mode string) string {
	if mode == "copy" {
		return "copied"
	}
	return "moved"
}

func destinationPath(cfg config, result processedFile) string {
	if !result.Date.Known {
		return filepath.Join(cfg.DestRoot, cfg.UnknownDir, result.File.Name)
	}

	return filepath.Join(
		cfg.DestRoot,
		fmt.Sprintf("%04d", result.Date.Year),
		fmt.Sprintf("%02d", int(result.Date.Month)),
		result.File.Name,
	)
}

func resolveDestination(path, conflict string) (string, bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return path, false, nil
		}
		return "", false, err
	}

	if conflict == "skip" {
		return path, true, nil
	}

	dir := filepath.Dir(path)
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	for i := 1; i < 10000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s_dup%04d%s", base, i, ext))
		if _, err := os.Stat(candidate); err != nil {
			if os.IsNotExist(err) {
				return candidate, false, nil
			}
			return "", false, err
		}
	}

	return "", false, fmt.Errorf("could not find available duplicate name for %s", path)
}

func copyFile(source, destination string, mode fs.FileMode, modTime time.Time) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}

	_, copyErr := io.CopyBuffer(out, in, make([]byte, 1024*1024))
	syncErr := out.Sync()
	closeErr := out.Close()

	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		if copyErr != nil {
			return copyErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}

	_ = os.Chtimes(destination, modTime, modTime)
	return nil
}

func moveFile(source, destination string, mode fs.FileMode, modTime time.Time) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	} else if !isCrossDeviceError(err) {
		return err
	}

	if err := copyFile(source, destination, mode, modTime); err != nil {
		return err
	}
	return os.Remove(source)
}

func isCrossDeviceError(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return errors.Is(linkErr.Err, syscall.EXDEV)
	}
	return errors.Is(err, syscall.EXDEV)
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(filepath.Clean(a))
	absB, errB := filepath.Abs(filepath.Clean(b))
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(absA, absB)
	}
	return absA == absB
}

func isSubpath(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func openReport(path string) (*os.File, *csv.Writer, error) {
	if path == "" || strings.EqualFold(path, "none") {
		return nil, nil, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, nil, err
	}

	file, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}

	return file, csv.NewWriter(file), nil
}

func writeReportHeader(writer *csv.Writer) error {
	return writer.Write([]string{
		"status",
		"action",
		"media_type",
		"source",
		"destination",
		"size_bytes",
		"date_source",
		"date_confidence",
		"date",
		"year",
		"month",
		"error",
	})
}

func writeReportRow(writer *csv.Writer, result processedFile, action actionResult) error {
	year := ""
	month := ""
	if result.Date.Known {
		year = fmt.Sprintf("%04d", result.Date.Year)
		month = fmt.Sprintf("%02d", int(result.Date.Month))
	}

	errText := ""
	if action.Error != nil {
		errText = action.Error.Error()
	}

	return writer.Write([]string{
		action.Status,
		action.Action,
		result.File.MediaType,
		result.File.Path,
		action.Destination,
		strconv.FormatInt(result.File.Size, 10),
		result.Date.Source,
		result.Date.Confidence,
		result.Date.Timestamp,
		year,
		month,
		errText,
	})
}

func (s *stats) record(result processedFile, action actionResult) {
	s.Processed++
	if result.File.MediaType == mediaImage {
		s.ImageFiles++
	} else if result.File.MediaType == mediaVideo {
		s.VideoFiles++
	}

	if result.Date.Known {
		s.DateSourceHit[result.Date.Source]++
		s.MonthHit[fmt.Sprintf("%04d/%02d", result.Date.Year, int(result.Date.Month))]++
	} else {
		s.UnknownDates++
		s.DateSourceHit["unknown"]++
	}

	switch action.Status {
	case "planned":
		s.Planned++
		s.BytesPlanned += result.File.Size
	case "moved":
		s.Moved++
		s.BytesMoved += result.File.Size
	case "copyd":
		s.Copied++
		s.BytesCopied += result.File.Size
	case "copied":
		s.Copied++
		s.BytesCopied += result.File.Size
	case "failed":
		s.Failed++
	default:
		s.Skipped++
	}
}

func printAction(result processedFile, action actionResult) {
	dateLabel := "unknown"
	if result.Date.Known {
		dateLabel = fmt.Sprintf("%04d/%02d via %s", result.Date.Year, int(result.Date.Month), result.Date.Source)
	}
	if action.Error != nil {
		fmt.Printf("%s: %s -> %s (%s): %v\n", action.Status, result.File.Path, action.Destination, dateLabel, action.Error)
		return
	}
	fmt.Printf("%s: %s -> %s (%s)\n", action.Action, result.File.Path, action.Destination, dateLabel)
}

func printSummary(cfg config, s stats, scanned int64) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 34))
	fmt.Println("Summary")
	fmt.Println(strings.Repeat("=", 34))
	fmt.Printf("Scanned media files: %d\n", scanned)
	fmt.Printf("Processed:           %d\n", s.Processed)
	fmt.Printf("Images:              %d\n", s.ImageFiles)
	fmt.Printf("Videos:              %d\n", s.VideoFiles)
	if cfg.Apply {
		fmt.Printf("Moved:               %d (%s)\n", s.Moved, humanBytes(s.BytesMoved))
		fmt.Printf("Copied:              %d (%s)\n", s.Copied, humanBytes(s.BytesCopied))
	} else {
		fmt.Printf("Planned:             %d (%s)\n", s.Planned, humanBytes(s.BytesPlanned))
	}
	fmt.Printf("Skipped:             %d\n", s.Skipped)
	fmt.Printf("Failed:              %d\n", s.Failed)
	fmt.Printf("Unknown dates:       %d\n", s.UnknownDates)
	if cfg.ReportPath != "" && !strings.EqualFold(cfg.ReportPath, "none") {
		fmt.Printf("CSV manifest:        %s\n", cfg.ReportPath)
	}

	if len(s.DateSourceHit) > 0 {
		fmt.Println()
		fmt.Println("Date sources:")
		for _, item := range sortedCounts(s.DateSourceHit) {
			fmt.Printf("  %-28s %d\n", item.Key, item.Value)
		}
	}

	if len(s.MonthHit) > 0 {
		fmt.Println()
		fmt.Println("Top months:")
		items := sortedCounts(s.MonthHit)
		limit := 12
		if len(items) < limit {
			limit = len(items)
		}
		for i := 0; i < limit; i++ {
			fmt.Printf("  %-8s %d\n", items[i].Key, items[i].Value)
		}
	}

	if !cfg.Apply {
		fmt.Println()
		fmt.Println("Dry-run only. Re-run with -apply after reviewing the CSV manifest.")
	}
}

type countItem struct {
	Key   string
	Value int64
}

func sortedCounts(values map[string]int64) []countItem {
	items := make([]countItem, 0, len(values))
	for key, value := range values {
		items = append(items, countItem{Key: key, Value: value})
	}

	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].Value > items[i].Value || (items[j].Value == items[i].Value && items[j].Key < items[i].Key) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	return items
}

func humanBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KB", "MB", "GB", "TB", "PB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f EB", value/unit)
}
