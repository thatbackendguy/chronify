package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
)

type processedFile struct {
	File mediaFile
	Date dateInfo
}

type plannedItem struct {
	File    mediaFile
	Date    dateInfo
	Action  actionResult
	Renamed bool
}

type plan struct {
	Items    []plannedItem
	Scanned  int64
	Reserved reservations
}

// buildPlan scans the source, detects dates in parallel and resolves every
// destination. Nothing on disk is changed.
func buildPlan(ctx context.Context, cfg config, tools metadataTools, prog *progress) (plan, error) {
	jobs := make(chan mediaFile, cfg.Workers*4)
	results := make(chan processedFile, cfg.Workers*4)
	var scanned int64
	scanErr := make(chan error, 1)

	var wg sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range jobs {
				if ctx.Err() != nil {
					continue
				}
				results <- processedFile{File: file, Date: determineDate(file, cfg, tools)}
			}
		}()
	}

	go func() {
		scanErr <- scanMediaFiles(ctx, cfg, jobs, &scanned)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	var processed []processedFile
	for result := range results {
		processed = append(processed, result)
		prog.planTick(int64(len(processed)), atomic.LoadInt64(&scanned))
	}
	prog.finish()

	if err := ctx.Err(); err != nil {
		return plan{}, errInterrupted
	}
	if err := <-scanErr; err != nil {
		return plan{}, err
	}

	// Workers finish in random order; sorting makes duplicate names
	// (photo_dup0001.jpg …) identical between a dry-run and the real run.
	sort.Slice(processed, func(i, j int) bool { return processed[i].File.Path < processed[j].File.Path })

	p := plan{
		Items:    make([]plannedItem, 0, len(processed)),
		Scanned:  atomic.LoadInt64(&scanned),
		Reserved: reservations{},
	}

	// Files already in their final place keep their names, so they claim
	// their paths before anything else is planned.
	for _, result := range processed {
		if dest := destinationPath(cfg, result); samePath(result.File.Path, dest) {
			p.Reserved.reserve(dest)
		}
	}

	for _, result := range processed {
		p.Items = append(p.Items, planItem(cfg, result, p.Reserved))
	}
	return p, nil
}

func planItem(cfg config, result processedFile, reserved reservations) plannedItem {
	item := plannedItem{File: result.File, Date: result.Date}
	dest := destinationPath(cfg, result)

	if samePath(result.File.Path, dest) {
		item.Action = actionResult{Action: actionSkip, Status: statusInPlace, Destination: dest}
		return item
	}

	final, skipped, err := resolveDestination(dest, cfg.Conflict, reserved)
	switch {
	case err != nil:
		item.Action = actionResult{Action: cfg.Mode, Status: statusFailed, Destination: dest, Error: err}
	case skipped:
		item.Action = actionResult{Action: actionSkip, Status: statusDestExists, Destination: dest}
	default:
		reserved.reserve(final)
		item.Renamed = final != dest
		item.Action = actionResult{Action: cfg.Mode, Status: statusPlanned, Destination: final}
	}
	return item
}

type planTotals struct {
	Planned, PlannedBytes int64
	InPlace, Exists       int64
	Renamed, Failed       int64
	Unknown               int64
}

func (p plan) totals() planTotals {
	var t planTotals
	for _, item := range p.Items {
		switch item.Action.Status {
		case statusPlanned:
			t.Planned++
			t.PlannedBytes += item.File.Size
			if item.Renamed {
				t.Renamed++
			}
			if !item.Date.Known {
				t.Unknown++
			}
		case statusInPlace:
			t.InPlace++
		case statusDestExists:
			t.Exists++
		case statusFailed:
			t.Failed++
		}
	}
	return t
}

type folderNode struct {
	Label    string
	Order    int
	Files    int64
	Bytes    int64
	Children map[string]*folderNode
}

func (n *folderNode) add(label string, order int, size int64) *folderNode {
	if n.Children == nil {
		n.Children = map[string]*folderNode{}
	}
	child, ok := n.Children[label]
	if !ok {
		child = &folderNode{Label: label, Order: order}
		n.Children[label] = child
	}
	child.Files++
	child.Bytes += size
	return child
}

func (n *folderNode) sorted() []*folderNode {
	nodes := make([]*folderNode, 0, len(n.Children))
	for _, child := range n.Children {
		nodes = append(nodes, child)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Order != nodes[j].Order {
			return nodes[i].Order < nodes[j].Order
		}
		return nodes[i].Label < nodes[j].Label
	})
	return nodes
}

// buildFolderTree groups planned files by year (and month) folder.
func buildFolderTree(cfg config, p plan) *folderNode {
	root := &folderNode{}
	for _, item := range p.Items {
		if item.Action.Status != statusPlanned {
			continue
		}
		parts := folderParts(item.Date, cfg)
		if !item.Date.Known {
			root.add(parts[0], 1<<30, item.File.Size)
			continue
		}
		year := root.add(parts[0], item.Date.Year, item.File.Size)
		if len(parts) > 1 {
			year.add(parts[1], int(item.Date.Month), item.File.Size)
		}
	}
	return root
}

// printPreview shows where files will go as a year (→ month) tree.
func printPreview(w io.Writer, cfg config, p plan) {
	root := buildFolderTree(cfg, p)
	t := p.totals()
	verb := capitalize(cfg.Mode)
	fmt.Fprintln(w, ui.bold("Preview"))
	if t.Planned == 0 {
		fmt.Fprintln(w, "  Nothing to "+cfg.Mode+".")
	} else {
		fmt.Fprintf(w, "  %s %s (%s) into %s\n\n", verb, countFiles(t.Planned), humanBytes(t.PlannedBytes), cfg.DestRoot)

		years := root.sorted()
		showMonths := cfg.By != layoutYear && (len(years) <= 3 || cfg.Verbose)
		rows := [][3]string{}
		for _, year := range years {
			rows = append(rows, [3]string{year.Label + string(filepath.Separator), countFiles(year.Files), humanBytes(year.Bytes)})
			if showMonths {
				for _, month := range year.sorted() {
					rows = append(rows, [3]string{"  " + month.Label + string(filepath.Separator), countFiles(month.Files), humanBytes(month.Bytes)})
				}
			}
		}
		printRows(w, "  ", rows)
		if cfg.By != layoutYear && !showMonths {
			fmt.Fprintln(w, ui.dim("  (month folders hidden for long previews; use -verbose to list them)"))
		}
	}

	notes := [][2]string{}
	if t.Unknown > 0 {
		notes = append(notes, [2]string{"No usable date", fmt.Sprintf("%s → %s%c", countFiles(t.Unknown), cfg.UnknownDir, filepath.Separator)})
	}
	if t.Renamed > 0 {
		notes = append(notes, [2]string{"Name clashes", countFiles(t.Renamed) + " renamed with a _dupNNNN suffix"})
	}
	if t.InPlace > 0 {
		notes = append(notes, [2]string{"Already in place", countFiles(t.InPlace)})
	}
	if t.Exists > 0 {
		notes = append(notes, [2]string{"Skipped (exists)", countFiles(t.Exists)})
	}
	if t.Failed > 0 {
		notes = append(notes, [2]string{"Cannot plan", ui.red(countFiles(t.Failed) + " (see manifest)")})
	}
	if len(notes) > 0 {
		fmt.Fprintln(w)
		for _, note := range notes {
			fmt.Fprintf(w, "  %-18s %s\n", note[0]+":", note[1])
		}
	}
	fmt.Fprintln(w)
}

// printRows prints a left-aligned label column followed by right-aligned
// count and size columns.
func printRows(w io.Writer, indent string, rows [][3]string) {
	widths := [3]int{}
	for _, row := range rows {
		for i, cell := range row {
			if n := len([]rune(cell)); n > widths[i] {
				widths[i] = n
			}
		}
	}
	for _, row := range rows {
		fmt.Fprintf(w, "%s%-*s  %*s  %*s\n", indent, widths[0], row[0], widths[1], row[1], widths[2], row[2])
	}
}
