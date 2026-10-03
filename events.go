package main

import (
	"encoding/json"
	"io"
	"os"
	"sync"
)

// stdout receives all human-readable output. In -json mode it is discarded
// and stdout carries one JSON event per line instead, for the macOS app and
// other front ends.
var stdout io.Writer = os.Stdout

type eventWriter struct {
	mu      sync.Mutex
	enc     *json.Encoder
	enabled bool
}

var events = &eventWriter{}

func enableJSONEvents() {
	stdout = io.Discard
	events = &eventWriter{enc: json.NewEncoder(os.Stdout), enabled: true}
}

// jsonRequested reports whether -json appears before the folder arguments,
// so even configuration errors can be reported as events.
func jsonRequested(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "-json", "--json", "-json=true", "--json=true":
			return true
		case "--":
			return false
		}
	}
	return false
}

func emit(event any) {
	if !events.enabled {
		return
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	_ = events.enc.Encode(event)
}

type scanEvent struct {
	Event string `json:"event"` // "scan"
	Dated int64  `json:"dated"`
	Found int64  `json:"found"`
}

type folderJSON struct {
	Name     string       `json:"name"`
	Files    int64        `json:"files"`
	Bytes    int64        `json:"bytes"`
	Children []folderJSON `json:"children,omitempty"`
}

type planEvent struct {
	Event        string       `json:"event"` // "plan"
	Mode         string       `json:"mode"`
	Apply        bool         `json:"apply"`
	Source       string       `json:"source"`
	Destination  string       `json:"destination"`
	Found        int64        `json:"found"`
	Planned      int64        `json:"planned"`
	PlannedBytes int64        `json:"planned_bytes"`
	InPlace      int64        `json:"in_place"`
	Exists       int64        `json:"exists"`
	Renamed      int64        `json:"renamed"`
	Failed       int64        `json:"failed"`
	Unknown      int64        `json:"unknown"`
	UnknownDir   string       `json:"unknown_dir"`
	Folders      []folderJSON `json:"folders"`
}

type progressEvent struct {
	Event      string `json:"event"` // "progress"
	Done       int64  `json:"done"`
	Total      int64  `json:"total"`
	DoneBytes  int64  `json:"done_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

type fileFailedEvent struct {
	Event       string `json:"event"` // "file_failed"
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Error       string `json:"error"`
}

type summaryEvent struct {
	Event        string `json:"event"` // "summary"
	Apply        bool   `json:"apply"`
	Mode         string `json:"mode"`
	Found        int64  `json:"found"`
	Planned      int64  `json:"planned"`
	Moved        int64  `json:"moved"`
	Copied       int64  `json:"copied"`
	Bytes        int64  `json:"bytes"`
	Skipped      int64  `json:"skipped"`
	Failed       int64  `json:"failed"`
	Unknown      int64  `json:"unknown"`
	Report       string `json:"report,omitempty"`
	Interrupted  bool   `json:"interrupted"`
	NotProcessed int64  `json:"not_processed"`
}

type reasonCount struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

type undoPlanEvent struct {
	Event        string        `json:"event"` // "undo_plan"
	Restore      int64         `json:"restore"`
	RestoreBytes int64         `json:"restore_bytes"`
	Remove       int64         `json:"remove"`
	RemoveBytes  int64         `json:"remove_bytes"`
	Skipped      []reasonCount `json:"skipped"`
}

type undoSummaryEvent struct {
	Event        string `json:"event"` // "undo_summary"
	Restored     int64  `json:"restored"`
	Removed      int64  `json:"removed"`
	Failed       int64  `json:"failed"`
	Report       string `json:"report,omitempty"`
	Interrupted  bool   `json:"interrupted"`
	NotProcessed int64  `json:"not_processed"`
}

type errorEvent struct {
	Event   string `json:"event"` // "error"
	Message string `json:"message"`
}

func emitPlan(cfg config, p plan) {
	if !events.enabled {
		return
	}
	t := p.totals()
	emit(planEvent{
		Event: "plan", Mode: cfg.Mode, Apply: cfg.Apply,
		Source: cfg.SourceRoot, Destination: cfg.DestRoot,
		Found: p.Scanned, Planned: t.Planned, PlannedBytes: t.PlannedBytes,
		InPlace: t.InPlace, Exists: t.Exists, Renamed: t.Renamed,
		Failed: t.Failed, Unknown: t.Unknown, UnknownDir: cfg.UnknownDir,
		Folders: folderTreeJSON(buildFolderTree(cfg, p)),
	})
}

func folderTreeJSON(node *folderNode) []folderJSON {
	children := node.sorted()
	out := make([]folderJSON, 0, len(children))
	for _, child := range children {
		out = append(out, folderJSON{
			Name:     child.Label,
			Files:    child.Files,
			Bytes:    child.Bytes,
			Children: folderTreeJSON(child),
		})
	}
	return out
}

func emitSummary(cfg config, s stats, found, remaining int64) {
	if !events.enabled {
		return
	}
	report := ""
	if reportEnabled(cfg.ReportPath) {
		report = cfg.ReportPath
	}
	emit(summaryEvent{
		Event: "summary", Apply: cfg.Apply, Mode: cfg.Mode, Found: found,
		Planned: s.Planned, Moved: s.Moved, Copied: s.Copied,
		Bytes:   s.BytesMoved + s.BytesCopied + s.BytesPlanned,
		Skipped: s.Skipped, Failed: s.Failed, Unknown: s.UnknownDates,
		Report: report, Interrupted: remaining > 0, NotProcessed: remaining,
	})
}
