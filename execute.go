package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	statusPlanned     = "planned"
	statusMoved       = "moved"
	statusCopied      = "copied"
	statusFailed      = "failed"
	statusInPlace     = "already_in_place"
	statusDestExists  = "destination_exists"
	actionSkip        = "skip"
	tempFileSuffix    = ".chronify-tmp"
	maxDuplicateIndex = 10000
)

var errDestinationExists = errors.New("destination already exists")

type actionResult struct {
	Action      string
	Status      string
	Destination string
	Error       error
}

// reservations tracks destination paths already claimed by this run, so two
// source files with the same name never get planned to the same place.
type reservations map[string]struct{}

func (r reservations) key(path string) string {
	path = filepath.Clean(path)
	// The default filesystems on macOS and Windows are case-insensitive.
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func (r reservations) taken(path string) bool {
	if r == nil {
		return false
	}
	_, ok := r[r.key(path)]
	return ok
}

func (r reservations) reserve(path string) {
	if r != nil {
		r[r.key(path)] = struct{}{}
	}
}

// resolveDestination returns a free path for destination, applying the
// conflict policy. A path is busy if it exists on disk or is reserved.
func resolveDestination(path, conflict string, reserved reservations) (string, bool, error) {
	busy, err := pathBusy(path, reserved)
	if err != nil || !busy {
		return path, false, err
	}

	if conflict == "skip" {
		return path, true, nil
	}

	dir := filepath.Dir(path)
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	for i := 1; i < maxDuplicateIndex; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s_dup%04d%s", base, i, ext))
		busy, err := pathBusy(candidate, reserved)
		if err != nil {
			return "", false, err
		}
		if !busy {
			return candidate, false, nil
		}
	}

	return "", false, fmt.Errorf("could not find available duplicate name for %s", path)
}

func pathBusy(path string, reserved reservations) (bool, error) {
	if reserved.taken(path) {
		return true, nil
	}
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// executeItem performs the planned action for one file.
func executeItem(cfg config, item plannedItem, reserved reservations) actionResult {
	if item.Action.Status != statusPlanned {
		return item.Action
	}

	destination := item.Action.Destination
	fail := func(err error) actionResult {
		return actionResult{Action: cfg.Mode, Status: statusFailed, Destination: destination, Error: err}
	}

	// Something may have been written to the planned path since planning.
	if _, err := os.Lstat(destination); err == nil {
		final, skipped, err := resolveDestination(destination, cfg.Conflict, reserved)
		if err != nil {
			return fail(err)
		}
		if skipped {
			return actionResult{Action: actionSkip, Status: statusDestExists, Destination: destination}
		}
		destination = final
		reserved.reserve(destination)
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return fail(err)
	}

	var err error
	switch cfg.Mode {
	case "copy":
		err = copyFile(item.File.Path, destination, item.File.Mode, item.File.ModTime)
	case "move":
		err = moveFile(item.File.Path, destination, item.File.Mode, item.File.ModTime)
	}
	if err != nil {
		return fail(err)
	}

	return actionResult{Action: cfg.Mode, Status: pastTense(cfg.Mode), Destination: destination}
}

func pastTense(mode string) string {
	if mode == "copy" {
		return statusCopied
	}
	return statusMoved
}

// copyFile copies into a hidden temp file next to destination and only then
// gives it its final name, so an interrupted copy never leaves a truncated
// file that looks like a real photo.
func copyFile(source, destination string, mode fs.FileMode, modTime time.Time) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	temp := filepath.Join(filepath.Dir(destination), "."+filepath.Base(destination)+tempFileSuffix)
	// A leftover temp file can only come from an earlier interrupted run.
	_ = os.Remove(temp)

	out, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm()|0200)
	if err != nil {
		return err
	}

	_, copyErr := io.CopyBuffer(out, in, make([]byte, 1024*1024))
	syncErr := out.Sync()
	closeErr := out.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		_ = os.Remove(temp)
		return err
	}

	_ = os.Chtimes(temp, modTime, modTime)
	if err := commitTemp(temp, destination); err != nil {
		_ = os.Remove(temp)
		return err
	}
	_ = os.Chmod(destination, mode.Perm())
	return nil
}

// commitTemp gives temp its final name without ever overwriting an existing
// file. A hard link fails if the target exists; filesystems without hard
// links (exFAT, FAT32, some network shares) fall back to check-then-rename.
func commitTemp(temp, destination string) error {
	err := os.Link(temp, destination)
	if err == nil {
		return os.Remove(temp)
	}
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: %s", errDestinationExists, destination)
	}

	if _, statErr := os.Lstat(destination); statErr == nil {
		return fmt.Errorf("%w: %s", errDestinationExists, destination)
	}
	return os.Rename(temp, destination)
}

func moveFile(source, destination string, mode fs.FileMode, modTime time.Time) error {
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("%w: %s", errDestinationExists, destination)
	}

	err := os.Rename(source, destination)
	if err == nil || !isCrossDeviceError(err) {
		return err
	}

	sourceInfo, err := os.Stat(source)
	if err != nil {
		return err
	}
	if err := copyFile(source, destination, mode, modTime); err != nil {
		return err
	}

	// Never delete the original unless the copy is verifiably complete.
	destInfo, err := os.Stat(destination)
	if err != nil {
		return fmt.Errorf("could not verify copy, original kept: %w", err)
	}
	if destInfo.Size() != sourceInfo.Size() {
		_ = os.Remove(destination)
		return fmt.Errorf("copy size mismatch (%d != %d bytes), original kept", destInfo.Size(), sourceInfo.Size())
	}

	if err := os.Remove(source); err != nil {
		return fmt.Errorf("copied, but could not remove original: %w", err)
	}
	return nil
}

func isCrossDeviceError(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return errors.Is(linkErr.Err, syscall.EXDEV)
	}
	return errors.Is(err, syscall.EXDEV)
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(filepath.Clean(a))
	absB, errB := filepath.Abs(filepath.Clean(b))
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(absA, absB)
	}
	return absA == absB
}

func isSubpath(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
