package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

const (
	undoRestore    = "restore"
	undoRemoveCopy = "remove_copy"
	statusRestored = "restored"
	statusRemoved  = "removed"
	statusSkipped  = "skipped"
)

var undoReportHeader = []string{"status", "action", "path", "restore_to", "size_bytes", "reason"}

type manifestRow struct {
	Status      string
	Source      string
	Destination string
	Size        int64 // -1 when unknown
}

type undoItem struct {
	Row    manifestRow
	Action string
	Status string
	Reason string
}

// runUndo reverses a previous run from its CSV manifest: moved files go back
// to where they came from, and copies are removed while the originals exist.
func runUndo(ctx context.Context, cfg config) error {
	fmt.Fprintln(stdout, ui.bold("Chronify undo"))
	fmt.Fprintln(stdout)
	mode := ui.yellow("DRY RUN") + ui.dim(" (preview only, nothing will change)")
	if cfg.Apply {
		mode = ui.bold("APPLY")
	}
	fmt.Fprintf(stdout, "  Mode:     %s\n", mode)
	fmt.Fprintf(stdout, "  Manifest: %s\n\n", cfg.Undo)

	rows, err := readManifest(cfg.Undo)
	if err != nil {
		return err
	}

	items := planUndo(rows)
	var restoreCount, restoreBytes, removeCount, removeBytes int64
	skipReasons := map[string]int64{}
	for _, item := range items {
		switch {
		case item.Status == statusSkipped:
			skipReasons[item.Reason]++
		case item.Action == undoRestore:
			restoreCount++
			restoreBytes += max(item.Row.Size, 0)
		case item.Action == undoRemoveCopy:
			removeCount++
			removeBytes += max(item.Row.Size, 0)
		}
	}

	skipped := []reasonCount{}
	for _, reason := range sortedCounts(skipReasons) {
		skipped = append(skipped, reasonCount{Reason: reason.Key, Count: reason.Value})
	}
	emit(undoPlanEvent{Event: "undo_plan", Restore: restoreCount, RestoreBytes: restoreBytes, Remove: removeCount, RemoveBytes: removeBytes, Skipped: skipped})

	fmt.Fprintln(stdout, ui.bold("Preview"))
	if restoreCount+removeCount+int64(len(skipReasons)) == 0 {
		fmt.Fprintln(stdout, "  The manifest has no moved or copied files to undo (was it a dry run?).")
		return nil
	}
	if restoreCount > 0 {
		fmt.Fprintf(stdout, "  Move back:     %s (%s) to their original folders\n", countFiles(restoreCount), humanBytes(restoreBytes))
	}
	if removeCount > 0 {
		fmt.Fprintf(stdout, "  Delete copies: %s (%s); originals are still in place\n", countFiles(removeCount), humanBytes(removeBytes))
	}
	for _, reason := range sortedCounts(skipReasons) {
		fmt.Fprintf(stdout, "  Skip:          %s — %s\n", countFiles(reason.Value), reason.Key)
	}
	fmt.Fprintln(stdout)

	total := restoreCount + removeCount
	if total == 0 {
		fmt.Fprintln(stdout, "Nothing can be undone.")
		return nil
	}

	if !cfg.Apply {
		rep, err := openReport(cfg.ReportPath, undoReportHeader)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := writeUndoRow(rep, item, item.Status, item.Reason); err != nil {
				rep.Close()
				return err
			}
		}
		if err := rep.Close(); err != nil {
			return err
		}
		if reportEnabled(cfg.ReportPath) {
			fmt.Fprintf(stdout, "Undo plan written to %s\n", cfg.ReportPath)
		}
		fmt.Fprintln(stdout, ui.yellow("Dry run only — nothing was changed.")+" Re-run with "+ui.bold("-apply")+" to undo.")
		return nil
	}

	if !cfg.Yes {
		if !isTerminal(os.Stdin) {
			return fmt.Errorf("refusing to undo without confirmation because input is not a terminal; re-run with -yes")
		}
		ok, err := confirm(ctx, fmt.Sprintf("Undo %s?", countFiles(total)))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(stdout, "Cancelled. Nothing was changed.")
			return nil
		}
		fmt.Fprintln(stdout)
	}

	rep, err := openReport(cfg.ReportPath, undoReportHeader)
	if err != nil {
		return err
	}
	prog := newProgress(cfg)
	prog.startExec(total, restoreBytes+removeBytes)

	var done, doneBytes, restored, removed, failed, remaining int64
	interrupted := false
	for i, item := range items {
		if ctx.Err() != nil {
			interrupted = true
			remaining = int64(len(items) - i)
			break
		}

		status, reason := item.Status, item.Reason
		if item.Status == statusPlanned {
			var err error
			status, err = applyUndo(item, cfg.UnknownDir)
			switch {
			case err != nil:
				reason = err.Error()
				failed++
				emit(fileFailedEvent{Event: "file_failed", Source: item.Row.Destination, Destination: item.Row.Source, Error: reason})
				prog.clear()
				fmt.Fprintf(stdout, "%s %s: %v\n", ui.red("failed:"), item.Row.Destination, err)
			case status == statusRestored:
				restored++
			case status == statusRemoved:
				removed++
			}
			done++
			doneBytes += max(item.Row.Size, 0)
		}

		if err := writeUndoRow(rep, item, status, reason); err != nil {
			prog.finish()
			rep.Close()
			return fmt.Errorf("writing undo manifest: %w", err)
		}
		if cfg.Verbose && item.Status == statusPlanned {
			prog.clear()
			fmt.Fprintf(stdout, "%s: %s\n", status, item.Row.Destination)
		}
		prog.execTick(done, doneBytes, false)
	}
	prog.execTick(done, doneBytes, true)
	prog.finish()
	if err := rep.Close(); err != nil {
		return err
	}

	undoReport := ""
	if reportEnabled(cfg.ReportPath) {
		undoReport = cfg.ReportPath
	}
	emit(undoSummaryEvent{Event: "undo_summary", Restored: restored, Removed: removed, Failed: failed, Report: undoReport, Interrupted: interrupted, NotProcessed: remaining})

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, ui.bold("Summary"))
	fmt.Fprintf(stdout, "  %-18s %s\n", "Moved back:", ui.green(formatCount(restored)))
	fmt.Fprintf(stdout, "  %-18s %s\n", "Copies deleted:", ui.green(formatCount(removed)))
	fmt.Fprintf(stdout, "  %-18s %s\n", "Failed:", formatCount(failed))
	if remaining > 0 {
		fmt.Fprintf(stdout, "  %-18s %s\n", "Not processed:", ui.yellow(formatCount(remaining)+" (interrupted)"))
	}
	if reportEnabled(cfg.ReportPath) {
		fmt.Fprintf(stdout, "  %-18s %s\n", "Undo manifest:", cfg.ReportPath)
	}

	if interrupted {
		return errInterrupted
	}
	if failed > 0 {
		return failuresError{count: failed}
	}
	return nil
}

func readManifest(path string) ([]manifestRow, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading manifest header: %w", err)
	}

	columns := map[string]int{}
	for i, name := range header {
		columns[name] = i
	}
	for _, required := range []string{"status", "source", "destination"} {
		if _, ok := columns[required]; !ok {
			return nil, fmt.Errorf("%s is not a chronify manifest (missing %q column)", path, required)
		}
	}
	field := func(record []string, name string) string {
		if i, ok := columns[name]; ok && i < len(record) {
			return record[i]
		}
		return ""
	}

	var rows []manifestRow
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading manifest: %w", err)
		}
		size, err := strconv.ParseInt(field(record, "size_bytes"), 10, 64)
		if err != nil {
			size = -1
		}
		rows = append(rows, manifestRow{
			Status:      field(record, "status"),
			Source:      field(record, "source"),
			Destination: field(record, "destination"),
			Size:        size,
		})
	}
	return rows, nil
}

// planUndo checks every moved/copied row against the disk, newest first.
func planUndo(rows []manifestRow) []undoItem {
	var items []undoItem
	claimed := reservations{}
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		item := undoItem{Row: row, Status: statusPlanned}

		switch row.Status {
		case statusMoved:
			item.Action = undoRestore
			switch {
			case !fileMatches(row.Destination, row.Size):
				item.Status, item.Reason = statusSkipped, "file is no longer at its organized location (or was changed)"
			case claimed.taken(row.Source) || exists(row.Source):
				item.Status, item.Reason = statusSkipped, "another file already exists at the original location"
			default:
				claimed.reserve(row.Source)
			}
		case statusCopied:
			item.Action = undoRemoveCopy
			switch {
			case !fileMatches(row.Destination, row.Size):
				item.Status, item.Reason = statusSkipped, "copy is missing or was changed"
			case !fileMatches(row.Source, row.Size):
				item.Status, item.Reason = statusSkipped, "original is missing, so the copy is kept"
			}
		default:
			continue
		}
		items = append(items, item)
	}
	return items
}

func applyUndo(item undoItem, unknownDir string) (string, error) {
	switch item.Action {
	case undoRestore:
		info, err := os.Stat(item.Row.Destination)
		if err != nil {
			return statusFailed, err
		}
		if err := os.MkdirAll(filepath.Dir(item.Row.Source), 0755); err != nil {
			return statusFailed, err
		}
		if err := moveFile(item.Row.Destination, item.Row.Source, info.Mode(), info.ModTime()); err != nil {
			return statusFailed, err
		}
		pruneEmptyDateDirs(filepath.Dir(item.Row.Destination), unknownDir)
		return statusRestored, nil
	case undoRemoveCopy:
		if err := os.Remove(item.Row.Destination); err != nil {
			return statusFailed, err
		}
		pruneEmptyDateDirs(filepath.Dir(item.Row.Destination), unknownDir)
		return statusRemoved, nil
	}
	return statusFailed, fmt.Errorf("unknown undo action %q", item.Action)
}

func writeUndoRow(rep *report, item undoItem, status, reason string) error {
	action := item.Action
	restoreTo := ""
	if action == undoRestore {
		restoreTo = item.Row.Source
	}
	return rep.write([]string{status, action, item.Row.Destination, restoreTo, strconv.FormatInt(item.Row.Size, 10), reason})
}

func fileMatches(path string, size int64) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return size < 0 || info.Size() == size
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

var (
	numberFolderPattern = regexp.MustCompile(`^\d{2}$|^\d{4}$`)
	monthFolderNames    = func() map[string]struct{} {
		names := map[string]struct{}{}
		for m := time.January; m <= time.December; m++ {
			for _, format := range monthFormats {
				names[monthLabel(m, format)] = struct{}{}
			}
		}
		return names
	}()
	// Files the OS drops into folders that should not keep them alive.
	osClutterFiles = map[string]struct{}{".DS_Store": {}, "Thumbs.db": {}, "desktop.ini": {}}
)

// isDateFolderName reports whether name looks like a folder chronify creates.
func isDateFolderName(name, unknownDir string) bool {
	if name == unknownDir || numberFolderPattern.MatchString(name) {
		return true
	}
	_, ok := monthFolderNames[name]
	return ok
}

// pruneEmptyDateDirs removes dir and its parents while they are empty and
// look like chronify date folders (day → month → year).
func pruneEmptyDateDirs(dir, unknownDir string) {
	for i := 0; i < 3; i++ {
		if !isDateFolderName(filepath.Base(dir), unknownDir) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if _, ok := osClutterFiles[entry.Name()]; !ok {
				return
			}
		}
		for _, entry := range entries {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
