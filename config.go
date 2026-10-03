package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

// appVersion falls back to the module version Go records for
// "go install github.com/thatbackendguy/chronify@v1.2.3" builds.
func appVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
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
	By            string
	MonthFormat   string
	Undo          string
	Apply         bool
	Yes           bool
	UseModTime    bool
	IncludeHidden bool
	Verbose       bool
	NoColor       bool
	Interactive   bool
	ShowVersion   bool
	Workers       int
	MinYear       int
	MaxYear       int
	ProgressEvery int64

	reportSet bool
}

func defaultConfig() (config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config{}, err
	}

	now := time.Now()
	return config{
		SourceRoot:    cwd,
		ReportPath:    filepath.Join(cwd, fmt.Sprintf("organizer-report-%s.csv", now.Format("20060102-150405"))),
		UnknownDir:    "_unknown_date",
		Mode:          "move",
		Conflict:      "rename",
		MetadataMode:  "auto",
		MediaFilter:   "all",
		By:            layoutMonth,
		MonthFormat:   "number-long",
		UseModTime:    true,
		Workers:       defaultWorkers(),
		MinYear:       1900,
		MaxYear:       now.Year() + 1,
		ProgressEvery: 1000,
	}, nil
}

func parseConfig(args []string, output io.Writer) (config, error) {
	cfg, err := defaultConfig()
	if err != nil {
		return config{}, err
	}

	fs := flag.NewFlagSet("chronify", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() { printUsage(fs) }

	fs.StringVar(&cfg.SourceRoot, "source", cfg.SourceRoot, "folder to scan recursively (or pass it as the first argument)")
	fs.StringVar(&cfg.DestRoot, "dest", "", "folder where date folders are created; defaults to the source folder")
	fs.StringVar(&cfg.By, "by", cfg.By, "folder layout: year, month, or day")
	fs.StringVar(&cfg.MonthFormat, "month-format", cfg.MonthFormat, "month folder names: "+strings.Join(monthFormats, ", "))
	fs.BoolVar(&cfg.Apply, "apply", cfg.Apply, "actually move/copy files; omit for a dry-run preview")
	fs.StringVar(&cfg.Mode, "mode", cfg.Mode, "operation to perform when -apply is set: move or copy")
	fs.BoolVar(&cfg.Yes, "yes", cfg.Yes, "skip the confirmation prompt (for scripts)")
	fs.StringVar(&cfg.Undo, "undo", cfg.Undo, "reverse a previous run using its CSV manifest")
	fs.StringVar(&cfg.MediaFilter, "media", cfg.MediaFilter, "which files to include: all, images, or videos")
	fs.StringVar(&cfg.Conflict, "conflict", cfg.Conflict, "what to do when a file with the same name exists: rename or skip")
	fs.StringVar(&cfg.MetadataMode, "metadata", cfg.MetadataMode, "date metadata strategy: auto, native, or never")
	fs.BoolVar(&cfg.UseModTime, "modtime", cfg.UseModTime, "fall back to filesystem modified time when no capture date is found")
	fs.StringVar(&cfg.UnknownDir, "unknown-dir", cfg.UnknownDir, "folder for files with no usable date")
	fs.StringVar(&cfg.ReportPath, "report", cfg.ReportPath, `CSV manifest path; use "none" to disable`)
	fs.BoolVar(&cfg.IncludeHidden, "include-hidden", cfg.IncludeHidden, "include dot-prefixed files and folders")
	fs.BoolVar(&cfg.Verbose, "verbose", cfg.Verbose, "print every file action and the full month preview")
	fs.BoolVar(&cfg.NoColor, "no-color", cfg.NoColor, "disable colored output (NO_COLOR is also honored)")
	fs.BoolVar(&cfg.Interactive, "interactive", cfg.Interactive, "answer a few questions instead of passing flags")
	fs.IntVar(&cfg.Workers, "workers", cfg.Workers, "parallel date-detection workers")
	fs.IntVar(&cfg.MinYear, "min-year", cfg.MinYear, "minimum accepted media year")
	fs.IntVar(&cfg.MaxYear, "max-year", cfg.MaxYear, "maximum accepted media year")
	fs.Int64Var(&cfg.ProgressEvery, "progress-every", cfg.ProgressEvery, "when output is not a terminal, print progress every N files; 0 disables")
	fs.BoolVar(&cfg.ShowVersion, "version", cfg.ShowVersion, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.ShowVersion {
		return cfg, nil
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	cfg.reportSet = set["report"]

	positional := fs.Args()
	if len(positional) > 2 {
		return config{}, fmt.Errorf("expected at most SOURCE and DEST, got %d arguments (put flags before folders)", len(positional))
	}
	if len(positional) >= 1 {
		if set["source"] {
			return config{}, fmt.Errorf("give the source either as -source or as an argument, not both")
		}
		cfg.SourceRoot = positional[0]
	}
	if len(positional) == 2 {
		if set["dest"] {
			return config{}, fmt.Errorf("give the destination either as -dest or as an argument, not both")
		}
		cfg.DestRoot = positional[1]
	}

	if cfg.Undo != "" {
		return cfg, validateUndoConfig(&cfg)
	}
	return cfg, validateConfig(&cfg)
}

func printUsage(fs *flag.FlagSet) {
	out := fs.Output()
	fmt.Fprintf(out, `Chronify organizes photos and videos into date folders.

Usage:
  chronify                                   guided setup (in a terminal)
  chronify [flags] SOURCE [DEST]             preview, then add -apply to run
  chronify -undo REPORT.csv [-apply]         reverse a previous run

Layouts (-by, -month-format):
  -by year                                   %s
  -by month                                  %s
  -by day                                    %s
  -month-format number-short                 %s
  -month-format number                       %s
  -month-format long                         %s
  -month-format short                        %s

Examples:
  chronify ~/Pictures/import ~/Pictures/library
  chronify -by year ~/Pictures/import ~/Pictures/library
  chronify -apply -mode copy "/Volumes/SD Card" "/Volumes/Media Library"

Dry-run is the default: nothing changes until you pass -apply and confirm.

Flags:
`,
		layoutExample(layoutYear, ""),
		layoutExample(layoutMonth, "number-long"),
		layoutExample(layoutDay, "number-long"),
		layoutExample(layoutMonth, "number-short"),
		layoutExample(layoutMonth, "number"),
		layoutExample(layoutMonth, "long"),
		layoutExample(layoutMonth, "short"),
	)
	fs.PrintDefaults()
}

// validateConfig normalizes and checks an organize run's settings. The
// wizard calls it too, so both entry points accept the same values.
func validateConfig(cfg *config) error {
	var err error
	if cfg.SourceRoot, err = absPath(cfg.SourceRoot); err != nil {
		return err
	}
	if cfg.DestRoot == "" {
		cfg.DestRoot = cfg.SourceRoot
	}
	if cfg.DestRoot, err = absPath(cfg.DestRoot); err != nil {
		return err
	}
	if reportEnabled(cfg.ReportPath) {
		if cfg.ReportPath, err = absPath(cfg.ReportPath); err != nil {
			return err
		}
	}

	if cfg.Workers < 1 {
		return fmt.Errorf("-workers must be at least 1")
	}
	if cfg.MinYear < 1 || cfg.MaxYear < cfg.MinYear {
		return fmt.Errorf("year bounds are invalid")
	}

	checks := []struct {
		flag    string
		value   *string
		allowed []string
	}{
		{"by", &cfg.By, []string{layoutYear, layoutMonth, layoutDay}},
		{"month-format", &cfg.MonthFormat, monthFormats},
		{"mode", &cfg.Mode, []string{"move", "copy"}},
		{"conflict", &cfg.Conflict, []string{"rename", "skip"}},
		{"metadata", &cfg.MetadataMode, []string{"auto", "native", "never"}},
		{"media", &cfg.MediaFilter, []string{"all", "images", "videos"}},
	}
	for _, check := range checks {
		*check.value = strings.ToLower(strings.TrimSpace(*check.value))
		if !contains(check.allowed, *check.value) {
			return fmt.Errorf("-%s must be one of: %s (got %q)", check.flag, strings.Join(check.allowed, ", "), *check.value)
		}
	}

	unknown := filepath.Clean(strings.TrimSpace(cfg.UnknownDir))
	if unknown == "." || unknown == "" || filepath.IsAbs(unknown) || unknown == ".." || strings.HasPrefix(unknown, ".."+string(filepath.Separator)) {
		return fmt.Errorf("-unknown-dir must be a folder name inside the destination")
	}
	cfg.UnknownDir = unknown

	info, err := os.Stat(cfg.SourceRoot)
	if err != nil {
		return fmt.Errorf("source folder is not readable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a folder: %s", cfg.SourceRoot)
	}
	if info, err := os.Stat(cfg.DestRoot); err == nil && !info.IsDir() {
		return fmt.Errorf("destination is not a folder: %s", cfg.DestRoot)
	}

	return nil
}

func validateUndoConfig(cfg *config) error {
	var err error
	if cfg.Undo, err = absPath(cfg.Undo); err != nil {
		return err
	}
	if _, err := os.Stat(cfg.Undo); err != nil {
		return fmt.Errorf("cannot read manifest: %w", err)
	}
	if !cfg.reportSet {
		cfg.ReportPath = filepath.Join(filepath.Dir(cfg.ReportPath), fmt.Sprintf("chronify-undo-%s.csv", time.Now().Format("20060102-150405")))
	}
	if reportEnabled(cfg.ReportPath) {
		if cfg.ReportPath, err = absPath(cfg.ReportPath); err != nil {
			return err
		}
	}
	return nil
}

// absPath expands a leading "~" and makes the path absolute.
func absPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~"+string(filepath.Separator)) || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	return filepath.Abs(path)
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
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
