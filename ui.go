package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// terminalUI holds output settings shared by every printer.
type terminalUI struct {
	color bool
}

var ui = terminalUI{}

func initUI(cfg config) {
	_, noColorEnv := os.LookupEnv("NO_COLOR")
	ui.color = !cfg.NoColor && !cfg.JSON && !noColorEnv && isTerminal(os.Stdout) && os.Getenv("TERM") != "dumb"
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (u terminalUI) paint(code, s string) string {
	if !u.color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func (u terminalUI) bold(s string) string   { return u.paint("1", s) }
func (u terminalUI) dim(s string) string    { return u.paint("2", s) }
func (u terminalUI) red(s string) string    { return u.paint("31", s) }
func (u terminalUI) green(s string) string  { return u.paint("32", s) }
func (u terminalUI) yellow(s string) string { return u.paint("33", s) }
func (u terminalUI) cyan(s string) string   { return u.paint("36", s) }

// progress renders a single self-updating line on a terminal and falls back
// to occasional plain lines when output is piped or redirected.
type progress struct {
	w          io.Writer
	tty        bool
	json       bool
	every      int64
	start      time.Time
	lastDraw   time.Time
	drawn      bool
	frame      int
	totalFiles int64
	totalBytes int64
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func newProgress(cfg config) *progress {
	return &progress{
		w:     stdout,
		tty:   !events.enabled && isTerminal(os.Stdout),
		json:  events.enabled,
		every: cfg.ProgressEvery,
		start: time.Now(),
	}
}

func (p *progress) throttled() bool {
	now := time.Now()
	if p.drawn && now.Sub(p.lastDraw) < 100*time.Millisecond {
		return true
	}
	p.lastDraw = now
	return false
}

func (p *progress) draw(line string) {
	fmt.Fprint(p.w, "\r\033[K"+line)
	p.drawn = true
}

// clear removes the progress line so other output can be printed cleanly.
func (p *progress) clear() {
	if p != nil && p.tty && p.drawn {
		fmt.Fprint(p.w, "\r\033[K")
		p.drawn = false
	}
}

func (p *progress) finish() { p.clear() }

// jsonDue limits JSON progress events to about ten per second.
func (p *progress) jsonDue(force bool) bool {
	now := time.Now()
	if !force && now.Sub(p.lastDraw) < 100*time.Millisecond {
		return false
	}
	p.lastDraw = now
	return true
}

func (p *progress) planTick(processed, scanned int64) {
	if p == nil {
		return
	}
	if p.json {
		if p.jsonDue(false) {
			emit(scanEvent{Event: "scan", Dated: processed, Found: scanned})
		}
		return
	}
	if !p.tty {
		if p.every > 0 && processed%p.every == 0 {
			fmt.Fprintf(p.w, "Read dates for %s media files (found %s so far)\n", formatCount(processed), formatCount(scanned))
		}
		return
	}
	if p.throttled() {
		return
	}
	p.frame = (p.frame + 1) % len(spinnerFrames)
	p.draw(fmt.Sprintf("%s Reading dates… %s  %s",
		ui.cyan(spinnerFrames[p.frame]), countFiles(processed), ui.dim(rate(processed, time.Since(p.start)))))
}

func (p *progress) startExec(totalFiles, totalBytes int64) {
	p.start = time.Now()
	p.totalFiles = totalFiles
	p.totalBytes = totalBytes
	p.drawn = false
}

func (p *progress) execTick(doneFiles, doneBytes int64, force bool) {
	if p == nil {
		return
	}
	if p.json {
		if p.jsonDue(force) {
			emit(progressEvent{Event: "progress", Done: doneFiles, Total: p.totalFiles, DoneBytes: doneBytes, TotalBytes: p.totalBytes})
		}
		return
	}
	if !p.tty {
		if p.every > 0 && doneFiles%p.every == 0 {
			fmt.Fprintf(p.w, "Processed %s/%s files (%s/%s)\n", formatCount(doneFiles), formatCount(p.totalFiles), humanBytes(doneBytes), humanBytes(p.totalBytes))
		}
		return
	}
	if !force && p.throttled() {
		return
	}

	fraction := 1.0
	if p.totalBytes > 0 {
		fraction = float64(doneBytes) / float64(p.totalBytes)
	} else if p.totalFiles > 0 {
		fraction = float64(doneFiles) / float64(p.totalFiles)
	}
	const width = 24
	filled := int(fraction * width)
	if filled > width {
		filled = width
	}
	bar := ui.green(strings.Repeat("█", filled)) + ui.dim(strings.Repeat("░", width-filled))

	elapsed := time.Since(p.start)
	eta := ""
	if fraction > 0 && fraction < 1 && elapsed > time.Second {
		remaining := time.Duration(float64(elapsed) * (1 - fraction) / fraction)
		eta = "  ETA " + formatDuration(remaining)
	}

	p.draw(fmt.Sprintf("[%s] %3d%%  %s/%s  %s/%s  %s%s",
		bar, int(fraction*100),
		formatCount(doneFiles), formatCount(p.totalFiles),
		humanBytes(doneBytes), humanBytes(p.totalBytes),
		rate(doneFiles, elapsed), eta))
}

func rate(count int64, elapsed time.Duration) string {
	if elapsed < 500*time.Millisecond {
		return ""
	}
	return fmt.Sprintf("%.0f files/s", float64(count)/elapsed.Seconds())
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

var stdinReader = bufio.NewReader(os.Stdin)

// readLine reads one line from stdin, returning early if ctx is cancelled
// (Ctrl+C) so prompts never block an interrupt.
func readLine(ctx context.Context) (string, error) {
	type lineResult struct {
		text string
		err  error
	}
	ch := make(chan lineResult, 1)
	go func() {
		text, err := stdinReader.ReadString('\n')
		ch <- lineResult{text, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil && r.text == "" {
			return "", r.err
		}
		return strings.TrimSpace(r.text), nil
	case <-ctx.Done():
		return "", errInterrupted
	}
}

func confirm(ctx context.Context, question string) (bool, error) {
	fmt.Fprintf(stdout, "%s %s ", question, ui.dim("[y/N]"))
	answer, err := readLine(ctx)
	if err == io.EOF {
		fmt.Fprintln(stdout)
		return false, fmt.Errorf("no answer received (input was closed); re-run with -yes to skip the prompt")
	}
	if err != nil {
		fmt.Fprintln(stdout)
		return false, err
	}
	answer = strings.ToLower(answer)
	return answer == "y" || answer == "yes", nil
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
}

func newStats() stats {
	return stats{DateSourceHit: make(map[string]int64)}
}

func (s *stats) record(item plannedItem, action actionResult) {
	s.Processed++
	if item.File.MediaType == mediaImage {
		s.ImageFiles++
	} else if item.File.MediaType == mediaVideo {
		s.VideoFiles++
	}

	if item.Date.Known {
		s.DateSourceHit[item.Date.Source]++
	} else {
		s.UnknownDates++
		s.DateSourceHit["unknown"]++
	}

	switch action.Status {
	case statusPlanned:
		s.Planned++
		s.BytesPlanned += item.File.Size
	case statusMoved:
		s.Moved++
		s.BytesMoved += item.File.Size
	case statusCopied:
		s.Copied++
		s.BytesCopied += item.File.Size
	case statusFailed:
		s.Failed++
	default:
		s.Skipped++
	}
}

func printRunHeader(cfg config, tools metadataTools) {
	mode := ui.yellow("DRY RUN") + ui.dim(" (preview only, nothing will change)")
	if cfg.Apply {
		mode = ui.bold(strings.ToUpper(cfg.Mode))
	}

	fmt.Fprintln(stdout, ui.bold("Chronify")+ui.dim(" · photo & video organizer"))
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  Mode:        %s\n", mode)
	fmt.Fprintf(stdout, "  Source:      %s\n", cfg.SourceRoot)
	fmt.Fprintf(stdout, "  Destination: %s\n", cfg.DestRoot)
	fmt.Fprintf(stdout, "  Layout:      %s\n", layoutExample(cfg.By, cfg.MonthFormat))
	fmt.Fprintf(stdout, "  Media:       %s\n", cfg.MediaFilter)
	if cfg.Verbose {
		fmt.Fprintf(stdout, "  Workers:     %d\n", cfg.Workers)
		fmt.Fprintf(stdout, "  Years:       %d-%d\n", cfg.MinYear, cfg.MaxYear)
	}
	fmt.Fprintf(stdout, "  Metadata:    %s", cfg.MetadataMode)
	if cfg.MetadataMode == "auto" {
		fmt.Fprintf(stdout, ui.dim(" (exiftool %s, ffprobe %s, mdls %s)"), yesNo(tools.Exiftool != ""), yesNo(tools.FFprobe != ""), yesNo(tools.MDLS != ""))
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout)
}

func yesNo(value bool) string {
	if value {
		return "✓"
	}
	return "✗"
}

func printAction(item plannedItem, action actionResult, dryRun bool) {
	dateLabel := "unknown date"
	if item.Date.Known {
		dateLabel = fmt.Sprintf("%s via %s", item.Date.Timestamp, item.Date.Source)
	}
	if action.Error != nil {
		fmt.Fprintf(stdout, "%s %s -> %s (%s): %v\n", ui.red(action.Status+":"), item.File.Path, action.Destination, dateLabel, action.Error)
		return
	}
	name := action.Action
	if dryRun && action.Status == statusPlanned {
		name = "would " + name
	}
	fmt.Fprintf(stdout, "%s %s -> %s %s\n", name+":", item.File.Path, action.Destination, ui.dim("("+dateLabel+")"))
}

func printSummary(cfg config, s stats, scanned int64, remaining int64) {
	fmt.Fprintln(stdout, ui.bold("Summary"))
	rows := [][2]string{
		{"Media files found", formatCount(scanned)},
		{"Images / videos", formatCount(s.ImageFiles) + " / " + formatCount(s.VideoFiles)},
	}
	if cfg.Apply {
		if cfg.Mode == "move" {
			rows = append(rows, [2]string{"Moved", ui.green(formatCount(s.Moved)) + " (" + humanBytes(s.BytesMoved) + ")"})
		} else {
			rows = append(rows, [2]string{"Copied", ui.green(formatCount(s.Copied)) + " (" + humanBytes(s.BytesCopied) + ")"})
		}
	} else {
		rows = append(rows, [2]string{"Would " + cfg.Mode, formatCount(s.Planned) + " (" + humanBytes(s.BytesPlanned) + ")"})
	}
	rows = append(rows, [2]string{"Skipped", formatCount(s.Skipped)})
	failed := formatCount(s.Failed)
	if s.Failed > 0 {
		failed = ui.red(failed)
	}
	rows = append(rows, [2]string{"Failed", failed})
	rows = append(rows, [2]string{"Unknown dates", formatCount(s.UnknownDates)})
	if remaining > 0 {
		rows = append(rows, [2]string{"Not processed", ui.yellow(formatCount(remaining) + " (interrupted)")})
	}
	if reportEnabled(cfg.ReportPath) {
		rows = append(rows, [2]string{"CSV manifest", cfg.ReportPath})
	}
	for _, row := range rows {
		fmt.Fprintf(stdout, "  %-18s %s\n", row[0]+":", row[1])
	}

	if len(s.DateSourceHit) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, ui.bold("Date sources"))
		for _, item := range sortedCounts(s.DateSourceHit) {
			fmt.Fprintf(stdout, "  %-26s %s\n", item.Key, formatCount(item.Value))
		}
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
	sort.Slice(items, func(i, j int) bool {
		if items[i].Value != items[j].Value {
			return items[i].Value > items[j].Value
		}
		return items[i].Key < items[j].Key
	})
	return items
}

// countFiles renders "1 file" or "12,345 files".
func countFiles(n int64) string {
	if n == 1 {
		return "1 file"
	}
	return formatCount(n) + " files"
}

// formatCount renders 12345 as "12,345".
func formatCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return "-" + formatCount(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
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
