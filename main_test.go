package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func testConfig() config {
	return config{
		DestRoot:     filepath.Clean("/library"),
		UnknownDir:   "_unknown_date",
		Mode:         "move",
		Conflict:     "rename",
		MetadataMode: "never",
		UseModTime:   true,
		MinYear:      1900,
		MaxYear:      2100,
	}
}

func TestFilenameDate(t *testing.T) {
	cfg := testConfig()
	tests := []struct {
		name  string
		year  int
		month time.Month
	}{
		{name: "IMG_20250204_223211.jpg", year: 2025, month: time.February},
		{name: "PXL_20231203_092312345.MP.jpg", year: 2023, month: time.December},
		{name: "Screenshot 2024-05-09 at 10.03.22.png", year: 2024, month: time.May},
		{name: "family-trip_2021.07.mov", year: 2021, month: time.July},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, ok := filenameDate(tt.name, cfg)
			if !ok {
				t.Fatalf("expected date for %s", tt.name)
			}
			if info.Year != tt.year || info.Month != tt.month {
				t.Fatalf("got %04d/%02d, want %04d/%02d", info.Year, info.Month, tt.year, tt.month)
			}
		})
	}
}

func TestFilenameDateRejectsInvalidCalendarDate(t *testing.T) {
	if _, ok := filenameDate("IMG_20250231_223211.jpg", testConfig()); ok {
		t.Fatal("expected invalid date to be rejected")
	}
}

func TestDetermineDatePrioritizesMetadataBeforeFilenameForImagesAndVideos(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake executable helper uses a POSIX shell script")
	}

	dir := t.TempDir()
	fakeExiftool := filepath.Join(dir, "fake-exiftool")
	if err := os.WriteFile(fakeExiftool, []byte("#!/bin/sh\nprintf '2024:05:09 10:03:22\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.MetadataMode = "auto"
	cfg.UseModTime = false

	tests := []struct {
		name      string
		mediaType string
	}{
		{name: "IMG_20200102_030405.jpg", mediaType: mediaImage},
		{name: "VID_20200102_030405.mov", mediaType: mediaVideo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name)
			if err := os.WriteFile(path, []byte("fixture"), 0644); err != nil {
				t.Fatal(err)
			}

			info := determineDate(mediaFile{
				Path:      path,
				Name:      tt.name,
				MediaType: tt.mediaType,
			}, cfg, metadataTools{Exiftool: fakeExiftool})

			if !info.Known {
				t.Fatal("expected metadata date")
			}
			if info.Source != "exiftool-metadata" {
				t.Fatalf("got source %q, want exiftool-metadata", info.Source)
			}
			if info.Year != 2024 || info.Month != time.May {
				t.Fatalf("got %04d/%02d, want 2024/05", info.Year, info.Month)
			}
		})
	}
}

func TestDestinationPathKnownDate(t *testing.T) {
	cfg := testConfig()
	result := processedFile{
		File: mediaFile{Name: "IMG_20240505_120000.jpg"},
		Date: dateInfo{Known: true, Year: 2024, Month: time.May},
	}

	got := destinationPath(cfg, result)
	want := filepath.Join(cfg.DestRoot, "2024", "05", "IMG_20240505_120000.jpg")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDestinationPathUnknownDate(t *testing.T) {
	cfg := testConfig()
	result := processedFile{
		File: mediaFile{Name: "scan.jpg"},
		Date: dateInfo{Known: false},
	}

	got := destinationPath(cfg, result)
	want := filepath.Join(cfg.DestRoot, cfg.UnknownDir, "scan.jpg")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveDestinationRename(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "photo.jpg")
	if err := copyFileFixture(original); err != nil {
		t.Fatal(err)
	}

	got, skipped, err := resolveDestination(original, "rename")
	if err != nil {
		t.Fatal(err)
	}
	if skipped {
		t.Fatal("did not expect destination to be skipped")
	}

	want := filepath.Join(dir, "photo_dup0001.jpg")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMediaTypeFor(t *testing.T) {
	tests := map[string]string{
		"photo.HEIC": mediaImage,
		"raw.NEF":    mediaImage,
		"clip.MOV":   mediaVideo,
		"movie.m2ts": mediaVideo,
	}

	for name, want := range tests {
		got, ok := mediaTypeFor(name)
		if !ok {
			t.Fatalf("expected %s to be detected", name)
		}
		if got != want {
			t.Fatalf("got %s for %s, want %s", got, name, want)
		}
	}
}

func TestParseDateString(t *testing.T) {
	tests := []string{
		"2024:05:09 10:03:22",
		"2024-05-09T10:03:22Z",
		"kMDItemContentCreationDate = 2024-05-09 10:03:22 +0000",
	}

	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			got, ok := parseDateString(value)
			if !ok {
				t.Fatalf("expected %q to parse", value)
			}
			if got.Year() != 2024 || got.Month() != time.May {
				t.Fatalf("got %s", got)
			}
		})
	}
}

func copyFileFixture(path string) error {
	return os.WriteFile(path, []byte("fixture"), 0644)
}
