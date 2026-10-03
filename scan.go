package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// junkDirs are system/NAS folders that hold thumbnails or deleted files,
// never originals worth organizing.
var junkDirs = map[string]struct{}{
	"@eaDir":                    {},
	"#recycle":                  {},
	"#snapshot":                 {},
	"$RECYCLE.BIN":              {},
	"System Volume Information": {},
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

func scanMediaFiles(ctx context.Context, cfg config, jobs chan<- mediaFile, scanned *int64) error {
	defer close(jobs)

	source := filepath.Clean(cfg.SourceRoot)
	dest := filepath.Clean(cfg.DestRoot)
	skipDest := !samePath(source, dest) && isSubpath(dest, source)

	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
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
			if _, ok := junkDirs[entry.Name()]; ok {
				return filepath.SkipDir
			}
			return nil
		}

		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !cfg.IncludeHidden && isHidden(entry.Name()) {
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
		file := mediaFile{
			Path:      path,
			RelPath:   rel,
			Name:      entry.Name(),
			MediaType: mediaType,
			Size:      info.Size(),
			ModTime:   info.ModTime(),
			Mode:      info.Mode(),
		}
		select {
		case jobs <- file:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
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
