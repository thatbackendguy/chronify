package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

var errInterrupted = errors.New("interrupted")

// maxFilesError stops a run that is larger than -max-files.
type maxFilesError struct{ planned, limit int64 }

func (e maxFilesError) Error() string {
	return fmt.Sprintf("this run would process %d files, more than the limit of %d (-max-files); nothing was changed", e.planned, e.limit)
}

// failuresError reports that the run finished but some files failed.
type failuresError struct{ count int64 }

func (e failuresError) Error() string { return fmt.Sprintf("%d files failed", e.count) }

func main() {
	os.Exit(mainExitCode())
}

func mainExitCode() int {
	args := os.Args[1:]
	wizard := len(args) == 0 && isTerminal(os.Stdin) && isTerminal(os.Stdout)
	if jsonRequested(args) {
		enableJSONEvents()
	}

	cfg, err := parseConfig(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		emit(errorEvent{Event: "error", Message: "configuration error: " + err.Error()})
		fmt.Fprintf(os.Stderr, "Configuration error: %v\nRun with -h for help.\n", err)
		return 2
	}
	if cfg.ShowVersion {
		fmt.Fprintln(stdout, "chronify", appVersion())
		return 0
	}
	initUI(cfg)

	ctx, cancel := interruptContext()
	defer cancel()

	if wizard || cfg.Interactive {
		if cfg, err = runWizard(ctx, cfg); err != nil {
			return exitCode(err)
		}
	}

	if cfg.Undo != "" {
		err = runUndo(ctx, cfg)
	} else {
		err = run(ctx, cfg)
	}
	return exitCode(err)
}

func exitCode(err error) int {
	var failures failuresError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errInterrupted):
		fmt.Fprintln(os.Stderr, ui.yellow("Interrupted."))
		return 130
	case errors.As(err, &failures):
		return 1
	case errors.Is(err, io.EOF):
		fmt.Fprintln(os.Stderr, "\nInput closed before setup finished. Nothing was changed.")
		return 1
	default:
		emit(errorEvent{Event: "error", Message: err.Error()})
		fmt.Fprintf(os.Stderr, "%s %v\n", ui.red("Error:"), err)
		return 1
	}
}

// interruptContext is cancelled by the first Ctrl+C so the current file can
// finish and the manifest can be saved. A second Ctrl+C quits immediately.
func interruptContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-signals:
			fmt.Fprintln(os.Stderr, "\n"+ui.yellow("Stopping after the current file… (press Ctrl+C again to quit immediately)"))
			signal.Stop(signals)
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(signals)
		cancel()
	}
}

func run(ctx context.Context, cfg config) error {
	tools := discoverMetadataTools(cfg)
	printRunHeader(cfg, tools)

	prog := newProgress(cfg)
	p, err := buildPlan(ctx, cfg, tools, prog)
	if err != nil {
		if errors.Is(err, errInterrupted) {
			fmt.Fprintln(stdout, "Nothing was changed.")
		}
		return err
	}

	printPreview(stdout, cfg, p)
	emitPlan(cfg, p)
	totals := p.totals()

	if !cfg.Apply {
		return finishDryRun(cfg, p)
	}

	if totals.Planned == 0 {
		fmt.Fprintln(stdout, "Nothing to do.")
		emitSummary(cfg, newStats(), p.Scanned, 0)
		return nil
	}

	if cfg.MaxFiles >= 0 && totals.Planned > cfg.MaxFiles {
		return maxFilesError{planned: totals.Planned, limit: cfg.MaxFiles}
	}

	if !cfg.Yes {
		if !isTerminal(os.Stdin) {
			return fmt.Errorf("refusing to %s files without confirmation because input is not a terminal; re-run with -yes", cfg.Mode)
		}
		question := fmt.Sprintf("%s %s (%s)?", capitalize(cfg.Mode), countFiles(totals.Planned), humanBytes(totals.PlannedBytes))
		ok, err := confirm(ctx, question)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(stdout, "Cancelled. Nothing was changed.")
			return nil
		}
		fmt.Fprintln(stdout)
	}

	return execute(ctx, cfg, p, totals, prog)
}

func finishDryRun(cfg config, p plan) error {
	rep, err := openReport(cfg.ReportPath, reportHeader)
	if err != nil {
		return err
	}
	s := newStats()
	for _, item := range p.Items {
		s.record(item, item.Action)
		if cfg.Verbose {
			printAction(item, item.Action, true)
		}
		if err := rep.writeItem(item, item.Action, true); err != nil {
			rep.Close()
			return err
		}
	}
	if err := rep.Close(); err != nil {
		return err
	}

	if cfg.Verbose && len(p.Items) > 0 {
		fmt.Fprintln(stdout)
	}
	printSummary(cfg, s, p.Scanned, 0)
	emitSummary(cfg, s, p.Scanned, 0)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, ui.yellow("Dry run only — nothing was changed."))
	if p.totals().Planned > 0 {
		fmt.Fprintln(stdout, "Review the preview (and CSV manifest), then re-run with "+ui.bold("-apply")+".")
	}
	return nil
}

func execute(ctx context.Context, cfg config, p plan, totals planTotals, prog *progress) error {
	rep, err := openReport(cfg.ReportPath, reportHeader)
	if err != nil {
		return err
	}

	s := newStats()
	prog.startExec(totals.Planned, totals.PlannedBytes)
	var doneFiles, doneBytes, remaining int64
	interrupted := false

	for i, item := range p.Items {
		if ctx.Err() != nil {
			interrupted = true
			remaining = int64(len(p.Items) - i)
			break
		}

		action := executeItem(cfg, item, p.Reserved)
		s.record(item, action)
		if err := rep.writeItem(item, action, false); err != nil {
			prog.finish()
			rep.Close()
			return fmt.Errorf("writing manifest: %w", err)
		}

		if item.Action.Status == statusPlanned {
			doneFiles++
			doneBytes += item.File.Size
		}
		if cfg.Verbose || action.Status == statusFailed {
			prog.clear()
			printAction(item, action, false)
		}
		if action.Status == statusFailed {
			emit(fileFailedEvent{Event: "file_failed", Source: item.File.Path, Destination: action.Destination, Error: errorText(action.Error)})
		}
		prog.execTick(doneFiles, doneBytes, false)
	}
	prog.execTick(doneFiles, doneBytes, true)
	prog.finish()

	if err := rep.Close(); err != nil {
		return err
	}

	fmt.Fprintln(stdout)
	printSummary(cfg, s, p.Scanned, remaining)
	emitSummary(cfg, s, p.Scanned, remaining)
	if reportEnabled(cfg.ReportPath) && (s.Moved > 0 || s.Copied > 0) {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, ui.dim("To reverse this run: chronify -undo "+shellQuote(cfg.ReportPath)+" -apply"))
	}

	if interrupted {
		return errInterrupted
	}
	if s.Failed > 0 {
		return failuresError{count: s.Failed}
	}
	return nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
