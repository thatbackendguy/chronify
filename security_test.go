package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, rows ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.csv")
	content := "status,action,media_type,source,destination,size_bytes\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func undoConfig(manifest string) config {
	cfg := testConfig()
	cfg.Undo = manifest
	cfg.Apply, cfg.Yes = true, true
	cfg.ReportPath = "none"
	return cfg
}

// The audit's attack: a shared manifest that "restores" a downloaded file
// into a LaunchAgents folder, where it would run at login.
func TestUndoRefusesCraftedMoveManifest(t *testing.T) {
	root := t.TempDir()
	payload := filepath.Join(root, "Downloads", "IMG_0001.jpg")
	writeFixture(t, payload)
	target := filepath.Join(root, "Library", "LaunchAgents", "com.evil.plist")

	manifest := writeManifest(t, fmt.Sprintf("moved,move,image,%s,%s,7", target, payload))
	if err := runUndo(context.Background(), undoConfig(manifest)); err != nil {
		t.Fatal(err)
	}
	if exists(target) || !exists(payload) {
		t.Fatal("undo moved a file that Chronify never organized")
	}
}

func TestUndoRefusesToDeleteFilesOutsideDateFolders(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "a", "IMG_0001.jpg")
	victim := filepath.Join(root, "Documents", "IMG_0001.jpg")
	writeFixture(t, original)
	writeFixture(t, victim)

	manifest := writeManifest(t, fmt.Sprintf("copied,copy,image,%s,%s,7", original, victim))
	if err := runUndo(context.Background(), undoConfig(manifest)); err != nil {
		t.Fatal(err)
	}
	if !exists(victim) {
		t.Fatal("undo deleted a file outside Chronify's date folders")
	}
}

func TestUndoKeepsCopiesThatDifferFromTheOriginal(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "in", "IMG_0001.jpg")
	copied := filepath.Join(root, "out", "2024", "01 - January", "IMG_0001.jpg")
	writeFixture(t, original)
	if err := os.MkdirAll(filepath.Dir(copied), 0755); err != nil {
		t.Fatal(err)
	}
	// Same size, different content: an edited photo must not be deleted.
	if err := os.WriteFile(copied, []byte("FIXTURE"), 0644); err != nil {
		t.Fatal(err)
	}

	manifest := writeManifest(t, fmt.Sprintf("copied,copy,image,%s,%s,7", original, copied))
	err := runUndo(context.Background(), undoConfig(manifest))
	var failures failuresError
	if !errorsAs(err, &failures) {
		t.Fatalf("expected the differing copy to be reported as failed, got %v", err)
	}
	if !exists(copied) {
		t.Fatal("undo deleted a copy whose content differs from the original")
	}
}

func TestValidateUndoRow(t *testing.T) {
	month := "/lib/2024/01 - January"
	tests := []struct {
		source, dest string
		ok           bool
	}{
		{"/in/IMG_1.jpg", month + "/IMG_1.jpg", true},
		{"/in/IMG_1.jpg", month + "/IMG_1_dup0003.jpg", true},
		{"/in/IMG_1_dup0001.jpg", month + "/IMG_1_dup0001_dup0001.jpg", true},
		{"/in/IMG_1.jpg", "/lib/2024/IMG_1.jpg", true},
		{"/in/IMG_1.jpg", "/lib/_unknown_date/IMG_1.jpg", true},
		{"/in/IMG_1.jpg", month + "/IMG_2.jpg", false},
		{"/in/IMG_1.jpg", month + "/IMG_1_dupX.jpg", false},
		{"/in/IMG_1.jpg", month + "/IMG_1.png", false},
		{"/in/IMG_1.jpg", "/Users/me/Documents/IMG_1.jpg", false},
		{"/in/notes.txt", month + "/notes.txt", false},
		{"in/IMG_1.jpg", month + "/IMG_1.jpg", false},
		{"/in/../etc/IMG_1.jpg", month + "/IMG_1.jpg", false},
		{month + "/IMG_1.jpg", month + "/IMG_1.jpg", false},
	}
	for _, tt := range tests {
		err := validateUndoRow(manifestRow{Source: tt.source, Destination: tt.dest}, "_unknown_date")
		if (err == nil) != tt.ok {
			t.Errorf("%s -> %s: got err=%v, want ok=%v", tt.source, tt.dest, err, tt.ok)
		}
	}
}

// On case-insensitive disks (the macOS default), a file in a folder whose
// name differs only in case is already in place and must not be renamed.
func TestInPlaceRunIgnoresFolderNameCase(t *testing.T) {
	src := t.TempDir()
	if !caseInsensitive(t, src) {
		t.Skip("filesystem is case-sensitive")
	}
	file := filepath.Join(src, "2024", "01 - january", "IMG_20240105_101010.jpg")
	writeFixture(t, file)

	cfg := testConfig()
	cfg.SourceRoot, cfg.DestRoot = src, src
	cfg.Apply, cfg.Yes, cfg.UseModTime, cfg.Workers = true, true, false, 2
	cfg.ReportPath = "none"
	for i := 0; i < 2; i++ {
		if err := run(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	if !exists(file) {
		entries, _ := os.ReadDir(filepath.Dir(file))
		t.Fatalf("file was renamed; folder now has %v", entries)
	}
}

func caseInsensitive(t *testing.T, dir string) bool {
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.WriteFile(probe, nil, 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	return exists(filepath.Join(dir, "caseprobe"))
}
