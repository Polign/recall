package local

import (
	"os"
	"path/filepath"
)

// Home is the directory Recall keeps its own configuration and, when nothing
// names a database, its memory in: $RECALL_HOME, else $XDG_DATA_HOME/recall,
// else ~/.local/share/recall.
//
// Two older spellings keep working. $POLIGN_RECALL_HOME names the directory
// as it did before 0.23.0. And when neither variable is set, a setup saved
// in the old default, ~/.config/polign/recall, is the home, so an upgrade
// finds the memory it kept there. Nothing is moved.
//
// It returns "" when the user's home directory is unknown, rather than a
// relative path that would scatter configuration and a private key wherever
// the command happened to run.
func Home() string {
	if dir := os.Getenv("RECALL_HOME"); dir != "" {
		return dir
	}
	if dir := os.Getenv("POLIGN_RECALL_HOME"); dir != "" {
		return dir
	}
	if old := LegacyHome(); old != "" {
		if _, err := os.Stat(filepath.Join(old, "config.json")); err == nil {
			return old
		}
	}
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "recall")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "recall")
}

// LegacyHome is where setups before 0.23.0 lived, ~/.config/polign/recall,
// or "" when the user's home directory is unknown.
func LegacyHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "polign", "recall")
}
