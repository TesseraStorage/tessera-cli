package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// installerPathMarker is the comment install.sh appends to the one PATH line
// it writes into a shell startup file. Keep in sync with install.sh.
const installerPathMarker = "# added by the Tessera installer"

// pathLineEdit is one shell startup file that holds installer PATH lines for
// this binary's directory, with the file content after removing them.
type pathLineEdit struct {
	File    string
	Lines   []string // the marked lines that would be removed, as written
	NewData []byte
	Mode    os.FileMode
}

// installerStartupFiles lists every startup file install.sh can write to
// (see startup_file in install.sh), deduplicated.
func installerStartupFiles() []string {
	if runtime.GOOS == "windows" {
		return nil // install.ps1 edits the user PATH, not a startup file
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	var files []string
	if z := os.Getenv("ZDOTDIR"); z != "" {
		files = append(files, filepath.Join(z, ".zshrc"))
	}
	files = append(files,
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".bash_profile"),
		filepath.Join(home, ".profile"),
	)
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	files = append(files, filepath.Join(xdg, "fish", "config.fish"))
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// isInstallerPathLine reports whether line is exactly a PATH line install.sh
// writes for dir (the export form, or fish_add_path in config.fish).
func isInstallerPathLine(line, dir string) bool {
	l := strings.TrimSpace(line)
	return l == `export PATH="`+dir+`:$PATH" `+installerPathMarker ||
		l == `fish_add_path "`+dir+`" `+installerPathMarker
}

// removeInstallerPathLines returns data without the installer's PATH lines
// for dir, also dropping the blank line the installer put before each one.
func removeInstallerPathLines(data []byte, dir string) ([]byte, []string) {
	lines := strings.SplitAfter(string(data), "\n")
	var kept, removed []string
	for _, ln := range lines {
		if isInstallerPathLine(strings.TrimRight(ln, "\r\n"), dir) {
			removed = append(removed, strings.TrimRight(ln, "\r\n"))
			if n := len(kept); n > 0 && strings.TrimSpace(kept[n-1]) == "" {
				kept = kept[:n-1]
			}
			continue
		}
		kept = append(kept, ln)
	}
	return []byte(strings.Join(kept, "")), removed
}

// planPathLineEdits finds the installer PATH lines for the directory holding
// exe in the shell startup files. Lines for other directories, and lines the
// user wrote themselves, are left alone.
func planPathLineEdits(exe string) []pathLineEdit {
	dirs := []string{filepath.Dir(exe)}
	if r, err := filepath.EvalSymlinks(exe); err == nil && filepath.Dir(r) != dirs[0] {
		dirs = append(dirs, filepath.Dir(r))
	}
	var edits []pathLineEdit
	for _, f := range installerStartupFiles() {
		// Edit the real file behind a symlinked dotfile, not the link.
		if r, err := filepath.EvalSymlinks(f); err == nil {
			f = r
		}
		st, err := os.Stat(f)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil || !strings.Contains(string(data), installerPathMarker) {
			continue
		}
		var all []string
		for _, d := range dirs {
			var removed []string
			data, removed = removeInstallerPathLines(data, d)
			all = append(all, removed...)
		}
		if len(all) > 0 {
			edits = append(edits, pathLineEdit{File: f, Lines: all, NewData: data, Mode: st.Mode().Perm()})
		}
	}
	return edits
}
