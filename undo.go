package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

	items := planUndo(rows, cfg.UnknownDir)
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
func planUndo(rows []manifestRow, unknownDir string) []undoItem {
	var items []undoItem
	claimed := reservations{}
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		item := undoItem{Row: row, Status: statusPlanned}

		if row.Status == statusMoved || row.Status == statusCopied {
			if err := validateUndoRow(row, unknownDir); err != nil {
				item.Action = undoRestore
				if row.Status == statusCopied {
					item.Action = undoRemoveCopy
				}
				item.Status, item.Reason = statusSkipped, "not a file Chronify organized ("+err.Error()+")"
				items = append(items, item)
				continue
			}
		}

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
		// Only delete a copy that is byte-for-byte identical to the original.
		same, err := sameContent(item.Row.Source, item.Row.Destination)
		if err != nil {
			return statusFailed, err
		}
		if !same {
			return statusFailed, fmt.Errorf("the copy differs from the original, so it was kept")
		}
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

var dupSuffixPattern = regexp.MustCompile(`^_dup\d{4}$`)

// validateUndoRow makes sure a manifest row describes something Chronify
// could have done: a media file filed into a date folder under its own
// name (or that name plus a _dupNNNN suffix). Undo never acts on anything
// else, so a hand-made or shared manifest can't move or delete arbitrary
// files.
func validateUndoRow(row manifestRow, unknownDir string) error {
	source, dest := row.Source, row.Destination
	if !filepath.IsAbs(source) || !filepath.IsAbs(dest) ||
		filepath.Clean(source) != source || filepath.Clean(dest) != dest {
		return fmt.Errorf("paths must be absolute")
	}
	if source == dest {
		return fmt.Errorf("source and destination are the same")
	}
	if _, ok := mediaTypeFor(dest); !ok {
		return fmt.Errorf("not a photo or video")
	}
	if !isDateFolderName(filepath.Base(filepath.Dir(dest)), unknownDir) {
		return fmt.Errorf("not inside a date folder")
	}

	sourceName, destName := filepath.Base(source), filepath.Base(dest)
	if sourceName == destName {
		return nil
	}
	ext := filepath.Ext(sourceName)
	if filepath.Ext(destName) != ext {
		return fmt.Errorf("file name changed")
	}
	sourceStem := strings.TrimSuffix(sourceName, ext)
	destStem := strings.TrimSuffix(destName, ext)
	if !strings.HasPrefix(destStem, sourceStem) || !dupSuffixPattern.MatchString(destStem[len(sourceStem):]) {
		return fmt.Errorf("file name changed")
	}
	return nil
}

// sameContent compares two files byte by byte.
func sameContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()

	bufA := make([]byte, 1024*1024)
	bufB := make([]byte, 1024*1024)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		doneA := errA == io.EOF || errA == io.ErrUnexpectedEOF
		doneB := errB == io.EOF || errB == io.ErrUnexpectedEOF
		if errA != nil && !doneA {
			return false, errA
		}
		if errB != nil && !doneB {
			return false, errB
		}
		if doneA || doneB {
			return doneA && doneB, nil
		}
	}
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
