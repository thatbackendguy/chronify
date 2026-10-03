package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var reportHeader = []string{
	"status",
	"action",
	"media_type",
	"source",
	"destination",
	"size_bytes",
	"date_source",
	"date_confidence",
	"date",
	"year",
	"month",
	"error",
}

// report is a CSV manifest. Every row is flushed as it is written, so the
// manifest stays accurate (and usable by -undo) if a run is interrupted.
type report struct {
	file   *os.File
	writer *csv.Writer
}

func reportEnabled(path string) bool {
	return path != "" && !strings.EqualFold(path, "none")
}

func openReport(path string, header []string) (*report, error) {
	if !reportEnabled(path) {
		return nil, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}

	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	r := &report{file: file, writer: csv.NewWriter(file)}
	if err := r.write(header); err != nil {
		file.Close()
		return nil, err
	}
	return r, nil
}

func (r *report) write(row []string) error {
	if r == nil {
		return nil
	}
	if err := r.writer.Write(row); err != nil {
		return err
	}
	r.writer.Flush()
	return r.writer.Error()
}

func (r *report) Close() error {
	if r == nil {
		return nil
	}
	r.writer.Flush()
	if err := r.writer.Error(); err != nil {
		r.file.Close()
		return err
	}
	return r.file.Close()
}

func (r *report) writeItem(item plannedItem, action actionResult, dryRun bool) error {
	if r == nil {
		return nil
	}

	year := ""
	month := ""
	if item.Date.Known {
		year = fmt.Sprintf("%04d", item.Date.Year)
		month = fmt.Sprintf("%02d", int(item.Date.Month))
	}

	errText := ""
	if action.Error != nil {
		errText = action.Error.Error()
	}

	actionName := action.Action
	if dryRun && action.Status == statusPlanned {
		actionName = "would_" + actionName
	}

	return r.write([]string{
		action.Status,
		actionName,
		item.File.MediaType,
		item.File.Path,
		action.Destination,
		strconv.FormatInt(item.File.Size, 10),
		item.Date.Source,
		item.Date.Confidence,
		item.Date.Timestamp,
		year,
		month,
		errText,
	})
}
