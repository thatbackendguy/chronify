package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// runWizard asks for the main settings step by step. It is used when chronify
// is started without arguments in a terminal, or with -interactive.
func runWizard(ctx context.Context, cfg config) (config, error) {
	fmt.Fprintln(stdout, ui.bold("Chronify")+ui.dim(" · photo & video organizer"))
	fmt.Fprintln(stdout, ui.dim("Answer a few questions. Press Enter to accept the [default]. Nothing changes until you confirm."))
	fmt.Fprintln(stdout)

	var err error
	if cfg.SourceRoot, err = askFolder(ctx, "Folder with photos/videos to organize", cfg.SourceRoot, true); err != nil {
		return cfg, err
	}
	if cfg.DestRoot, err = askFolder(ctx, "Where should the organized folders go", cfg.SourceRoot, false); err != nil {
		return cfg, err
	}

	layouts := []string{layoutYear, layoutMonth, layoutDay}
	choice, err := askChoice(ctx, "How should files be grouped?", []string{
		"By year                " + ui.dim(layoutExample(layoutYear, cfg.MonthFormat)),
		"By year, then month    " + ui.dim(layoutExample(layoutMonth, cfg.MonthFormat)),
		"By year, month and day " + ui.dim(layoutExample(layoutDay, cfg.MonthFormat)),
	}, indexOf(layouts, cfg.By))
	if err != nil {
		return cfg, err
	}
	cfg.By = layouts[choice]

	if cfg.By != layoutYear {
		options := make([]string, len(monthFormats))
		for i, format := range monthFormats {
			options[i] = fmt.Sprintf("%-14s %s", monthLabel(1, format), ui.dim("("+format+")"))
		}
		choice, err := askChoice(ctx, "How should month folders be named?", options, indexOf(monthFormats, cfg.MonthFormat))
		if err != nil {
			return cfg, err
		}
		cfg.MonthFormat = monthFormats[choice]
	}

	modes := []string{"copy", "move"}
	copyHint, defaultMode := "(originals stay where they are; safest)", 0
	if samePath(cfg.SourceRoot, cfg.DestRoot) {
		// Copying into the folder being organized would leave every original
		// unsorted next to its copy, so Move is the sensible default here.
		copyHint, defaultMode = "(keeps a second copy of every file in the same folder)", 1
	}
	choice, err = askChoice(ctx, "Copy or move the files?", []string{
		"Copy " + ui.dim(copyHint),
		"Move " + ui.dim("(no extra disk space needed)"),
	}, defaultMode)
	if err != nil {
		return cfg, err
	}
	cfg.Mode = modes[choice]

	media := []string{"all", "images", "videos"}
	choice, err = askChoice(ctx, "Which files?", []string{"Photos and videos", "Photos only", "Videos only"}, indexOf(media, cfg.MediaFilter))
	if err != nil {
		return cfg, err
	}
	cfg.MediaFilter = media[choice]

	// The wizard always shows the preview and asks before changing anything.
	cfg.Apply = true
	cfg.Yes = false
	if err := validateConfig(&cfg); err != nil {
		return cfg, err
	}

	fmt.Fprintln(stdout, ui.dim("Next time you can run this directly:"))
	fmt.Fprintln(stdout, "  "+equivalentCommand(cfg))
	fmt.Fprintln(stdout)
	return cfg, nil
}

func askFolder(ctx context.Context, question, def string, mustExist bool) (string, error) {
	for {
		fmt.Fprintf(stdout, "%s %s: ", ui.bold(question), ui.dim("["+def+"]"))
		answer, err := readLine(ctx)
		if err != nil {
			return "", err
		}
		path := cleanInputPath(answer)
		if path == "" {
			path = def
		}
		path, err = absPath(path)
		if err != nil {
			fmt.Fprintln(stdout, ui.red("  "+err.Error()))
			continue
		}

		info, statErr := os.Stat(path)
		switch {
		case statErr == nil && !info.IsDir():
			fmt.Fprintln(stdout, ui.red("  That is a file, not a folder."))
			continue
		case statErr != nil && mustExist:
			fmt.Fprintln(stdout, ui.red("  Cannot open that folder: "+statErr.Error()))
			continue
		case statErr != nil:
			fmt.Fprintln(stdout, ui.dim("  It will be created."))
		}
		fmt.Fprintln(stdout)
		return path, nil
	}
}

// cleanInputPath undoes the quoting terminals add when a folder is dragged
// in: surrounding quotes and backslash-escaped spaces.
func cleanInputPath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	if os.PathSeparator == '/' {
		s = strings.ReplaceAll(s, `\ `, " ")
	}
	return s
}

func askChoice(ctx context.Context, question string, options []string, def int) (int, error) {
	if def < 0 || def >= len(options) {
		def = 0
	}
	fmt.Fprintln(stdout, ui.bold(question))
	for i, option := range options {
		marker := " "
		if i == def {
			marker = ui.cyan("›")
		}
		fmt.Fprintf(stdout, " %s %d) %s\n", marker, i+1, option)
	}
	for {
		fmt.Fprintf(stdout, "Choose %s: ", ui.dim(fmt.Sprintf("[%d]", def+1)))
		answer, err := readLine(ctx)
		if err != nil {
			return 0, err
		}
		if answer == "" {
			fmt.Fprintln(stdout)
			return def, nil
		}
		n, err := strconv.Atoi(answer)
		if err == nil && n >= 1 && n <= len(options) {
			fmt.Fprintln(stdout)
			return n - 1, nil
		}
		fmt.Fprintln(stdout, ui.red(fmt.Sprintf("  Please enter a number from 1 to %d.", len(options))))
	}
}

func equivalentCommand(cfg config) string {
	args := []string{"chronify", "-by", cfg.By}
	if cfg.By != layoutYear {
		args = append(args, "-month-format", cfg.MonthFormat)
	}
	args = append(args, "-mode", cfg.Mode)
	if cfg.MediaFilter != "all" {
		args = append(args, "-media", cfg.MediaFilter)
	}
	args = append(args, "-apply", shellQuote(cfg.SourceRoot))
	if cfg.DestRoot != cfg.SourceRoot {
		args = append(args, shellQuote(cfg.DestRoot))
	}
	return strings.Join(args, " ")
}

// shellQuote quotes a path for copy-pasting into a POSIX shell.
func shellQuote(s string) string {
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-~:+@", r)) {
			safe = false
			break
		}
	}
	if safe && s != "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func indexOf(values []string, value string) int {
	for i, v := range values {
		if v == value {
			return i
		}
	}
	return 0
}
