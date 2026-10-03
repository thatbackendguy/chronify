package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDestinationPathLayouts(t *testing.T) {
	result := processedFile{
		File: mediaFile{Name: "IMG.jpg"},
		Date: dateInfo{Known: true, Year: 2025, Month: time.January, Day: 7},
	}
	tests := []struct {
		by, monthFormat string
		want            []string
	}{
		{layoutYear, "number-long", []string{"2025"}},
		{layoutMonth, "number-long", []string{"2025", "01 - January"}},
		{layoutMonth, "number-short", []string{"2025", "01-Jan"}},
		{layoutMonth, "number", []string{"2025", "01"}},
		{layoutMonth, "long", []string{"2025", "January"}},
		{layoutMonth, "short", []string{"2025", "Jan"}},
		{layoutDay, "number-long", []string{"2025", "01 - January", "07"}},
		{layoutDay, "number", []string{"2025", "01", "07"}},
	}

	for _, tt := range tests {
		t.Run(tt.by+"/"+tt.monthFormat, func(t *testing.T) {
			cfg := testConfig()
			cfg.By, cfg.MonthFormat = tt.by, tt.monthFormat
			want := filepath.Join(append(append([]string{cfg.DestRoot}, tt.want...), "IMG.jpg")...)
			if got := destinationPath(cfg, result); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestPlanReservesDestinationsForSameNames(t *testing.T) {
	src := t.TempDir()
	for _, dir := range []string{"a", "b"} {
		writeFixture(t, filepath.Join(src, dir, "IMG_20240105_101010.jpg"))
	}

	cfg := testConfig()
	cfg.SourceRoot, cfg.DestRoot = src, filepath.Join(src, "library")
	cfg.Workers = 2

	p, err := buildPlan(context.Background(), cfg, metadataTools{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(p.Items))
	}

	month := filepath.Join(cfg.DestRoot, "2024", "01 - January")
	want := []string{
		filepath.Join(month, "IMG_20240105_101010.jpg"),
		filepath.Join(month, "IMG_20240105_101010_dup0001.jpg"),
	}
	for i, item := range p.Items {
		if item.Action.Status != statusPlanned || item.Action.Destination != want[i] {
			t.Fatalf("item %d: got %s -> %q, want planned -> %q", i, item.Action.Status, item.Action.Destination, want[i])
		}
	}
	if !p.Items[1].Renamed {
		t.Fatal("expected second item to be marked renamed")
	}
}

func TestCopyFileNeverOverwritesAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.jpg")
	dest := filepath.Join(dir, "dest.jpg")
	writeFixture(t, source)
	if err := os.WriteFile(dest, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}

	err := copyFile(source, dest, 0644, time.Now())
	if !errors.Is(err, errDestinationExists) {
		t.Fatalf("got error %v, want errDestinationExists", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "existing" {
		t.Fatalf("destination was overwritten: %q", data)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), tempFileSuffix) {
			t.Fatalf("temp file left behind: %s", entry.Name())
		}
	}
}

func TestCopyFilePreservesContentAndModTime(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.jpg")
	dest := filepath.Join(dir, "out", "dest.jpg")
	writeFixture(t, source)
	modTime := time.Date(2020, 5, 1, 10, 0, 0, 0, time.UTC)
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(source, dest, 0644, modTime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(modTime) {
		t.Fatalf("got mod time %s, want %s", info.ModTime(), modTime)
	}
	if data, _ := os.ReadFile(dest); string(data) != "fixture" {
		t.Fatalf("got content %q", data)
	}
}

func TestUTCTimesUseLocalDate(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("EST", -5*3600)
	defer func() { time.Local = saved }()

	// 03:30 UTC on Jan 1 is still New Year's Eve in EST.
	ts, ok := parseDateString("2025-01-01T03:30:00Z")
	if !ok {
		t.Fatal("expected timestamp to parse")
	}
	info, ok := dateInfoFromTime(ts, "ffprobe-creation-time", "high", testConfig())
	if !ok {
		t.Fatal("expected date")
	}
	if info.Year != 2024 || info.Month != time.December || info.Day != 31 {
		t.Fatalf("got %04d-%02d-%02d, want 2024-12-31", info.Year, info.Month, info.Day)
	}
}

func TestSplitExiftoolLine(t *testing.T) {
	tag, value := splitExiftoolLine("ModifyDate: 2024:05:09 10:03:22")
	if tag != "ModifyDate" || value != "2024:05:09 10:03:22" {
		t.Fatalf("got %q / %q", tag, value)
	}
	tag, value = splitExiftoolLine("2024:05:09 10:03:22")
	if tag != "" || value != "2024:05:09 10:03:22" {
		t.Fatalf("bare value: got %q / %q", tag, value)
	}
}

func TestMoveThenUndoRestoresEverything(t *testing.T) {
	src, dest, cfg := organizeFixture(t, "move")

	if err := run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(src, "trip", "IMG_20240105_101010.jpg")) {
		t.Fatal("expected file to be moved")
	}
	if !exists(filepath.Join(dest, "2024", "01 - January", "IMG_20240105_101010.jpg")) {
		t.Fatal("expected file at organized location")
	}

	cfg.Undo = cfg.ReportPath
	cfg.ReportPath = filepath.Join(t.TempDir(), "undo.csv")
	if err := runUndo(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	for _, rel := range fixtureFiles {
		if !exists(filepath.Join(src, rel)) {
			t.Fatalf("expected %s to be restored", rel)
		}
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 0 {
		t.Fatalf("expected empty date folders to be pruned, found %d entries", len(entries))
	}
}

func TestCopyThenUndoRemovesCopiesOnly(t *testing.T) {
	src, dest, cfg := organizeFixture(t, "copy")

	if err := run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(dest, "2025", "12 - December", "VID_20251231_235959.mov")
	if !exists(copied) {
		t.Fatal("expected copy at organized location")
	}

	cfg.Undo = cfg.ReportPath
	cfg.ReportPath = "none"
	if err := runUndo(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if exists(copied) {
		t.Fatal("expected copy to be removed")
	}
	for _, rel := range fixtureFiles {
		if !exists(filepath.Join(src, rel)) {
			t.Fatalf("original %s must be kept", rel)
		}
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	src, dest, cfg := organizeFixture(t, "move")
	cfg.Apply = false

	if err := run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, rel := range fixtureFiles {
		if !exists(filepath.Join(src, rel)) {
			t.Fatalf("dry run touched %s", rel)
		}
	}
	if exists(filepath.Join(dest, "2024")) {
		t.Fatal("dry run created folders")
	}
}

func TestParseConfigPositionalArguments(t *testing.T) {
	src := t.TempDir()
	dest := t.TempDir()

	cfg, err := parseConfig([]string{"-by", "year", src, dest}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceRoot != src || cfg.DestRoot != dest || cfg.By != layoutYear {
		t.Fatalf("got source=%q dest=%q by=%q", cfg.SourceRoot, cfg.DestRoot, cfg.By)
	}

	cfg, err = parseConfig([]string{src}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DestRoot != src {
		t.Fatalf("dest should default to source, got %q", cfg.DestRoot)
	}

	if _, err := parseConfig([]string{"-month-format", "roman", src}, os.Stderr); err == nil {
		t.Fatal("expected invalid -month-format to be rejected")
	}
}

func TestPruneEmptyDateDirsStopsAtNonDateFolders(t *testing.T) {
	root := t.TempDir()
	library := filepath.Join(root, "library")
	month := filepath.Join(library, "2024", "01 - January")
	if err := os.MkdirAll(month, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(month, ".DS_Store"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	pruneEmptyDateDirs(month, "_unknown_date")
	if exists(filepath.Join(library, "2024")) {
		t.Fatal("expected empty year folder to be removed")
	}
	if !exists(library) {
		t.Fatal("library folder must be kept")
	}
}

var fixtureFiles = []string{
	filepath.Join("trip", "IMG_20240105_101010.jpg"),
	filepath.Join("trip", "VID_20251231_235959.mov"),
	filepath.Join("other", "IMG_20240105_101010.jpg"),
}

func organizeFixture(t *testing.T, mode string) (string, string, config) {
	t.Helper()
	src := t.TempDir()
	dest := t.TempDir()
	for _, rel := range fixtureFiles {
		writeFixture(t, filepath.Join(src, rel))
	}

	cfg := testConfig()
	cfg.SourceRoot, cfg.DestRoot = src, dest
	cfg.Mode = mode
	cfg.Apply, cfg.Yes = true, true
	cfg.UseModTime = false
	cfg.Workers = 2
	cfg.ReportPath = filepath.Join(t.TempDir(), "report.csv")
	return src, dest, cfg
}

func writeFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestMaxFilesRefusesLargeRuns(t *testing.T) {
	src, _, cfg := organizeFixture(t, "move")
	cfg.MaxFiles = 2

	err := run(context.Background(), cfg)
	var limitErr maxFilesError
	if !errors.As(err, &limitErr) || limitErr.planned != 3 {
		t.Fatalf("got %v, want maxFilesError for 3 files", err)
	}
	for _, rel := range fixtureFiles {
		if !exists(filepath.Join(src, rel)) {
			t.Fatalf("%s was moved despite the limit", rel)
		}
	}

	cfg.MaxFiles = 0
	if err := run(context.Background(), cfg); !errors.As(err, &limitErr) {
		t.Fatalf("-max-files 0 must allow nothing, got %v", err)
	}

	cfg.MaxFiles = 3
	if err := run(context.Background(), cfg); err != nil {
		t.Fatalf("run within the limit failed: %v", err)
	}
}
