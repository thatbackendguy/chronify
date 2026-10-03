package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// captureEvents runs fn with JSON events enabled and returns them decoded.
func captureEvents(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	savedEvents, savedStdout := events, stdout
	events = &eventWriter{enc: json.NewEncoder(&buf), enabled: true}
	stdout = io.Discard
	defer func() { events, stdout = savedEvents, savedStdout }()

	fn()

	var out []map[string]any
	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("invalid JSON line %q: %v", scanner.Text(), err)
		}
		out = append(out, event)
	}
	return out
}

func findEvent(events []map[string]any, name string) map[string]any {
	for _, event := range events {
		if event["event"] == name {
			return event
		}
	}
	return nil
}

func TestJSONEventsForPreviewApplyAndUndo(t *testing.T) {
	src, dest, cfg := organizeFixture(t, "move")

	cfg.Apply = false
	preview := captureEvents(t, func() {
		if err := run(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	})
	plan := findEvent(preview, "plan")
	if plan == nil || plan["planned"] != float64(3) || plan["renamed"] != float64(1) {
		t.Fatalf("unexpected plan event: %v", plan)
	}
	folders := plan["folders"].([]any)
	first := folders[0].(map[string]any)
	if first["name"] != "2024" || first["files"] != float64(2) {
		t.Fatalf("unexpected first folder: %v", first)
	}
	month := first["children"].([]any)[0].(map[string]any)
	if month["name"] != "01 - January" {
		t.Fatalf("unexpected month folder: %v", month)
	}

	cfg.Apply = true
	applied := captureEvents(t, func() {
		if err := run(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	})
	summary := findEvent(applied, "summary")
	if summary == nil || summary["moved"] != float64(3) || summary["report"] != cfg.ReportPath {
		t.Fatalf("unexpected summary: %v", summary)
	}
	if findEvent(applied, "progress") == nil {
		t.Fatal("expected at least one progress event")
	}

	cfg.Undo = cfg.ReportPath
	cfg.ReportPath = "none"
	undone := captureEvents(t, func() {
		if err := runUndo(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	})
	if plan := findEvent(undone, "undo_plan"); plan == nil || plan["restore"] != float64(3) {
		t.Fatalf("unexpected undo plan: %v", plan)
	}
	if summary := findEvent(undone, "undo_summary"); summary == nil || summary["restored"] != float64(3) {
		t.Fatalf("unexpected undo summary: %v", summary)
	}
	if !exists(filepath.Join(src, fixtureFiles[0])) {
		t.Fatal("expected files restored")
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatal("expected destination cleaned up")
	}
}

func TestJSONRequested(t *testing.T) {
	if !jsonRequested([]string{"-by", "year", "-json", "src"}) {
		t.Fatal("expected -json to be detected")
	}
	if jsonRequested([]string{"--", "-json"}) {
		t.Fatal("arguments after -- are not flags")
	}
}
