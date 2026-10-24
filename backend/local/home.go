package local

import "github.com/Polign/recall/internal/recallhome"

// Home is the directory Recall keeps its own configuration and, when nothing
// names a database, its memory in: $RECALL_HOME, else $XDG_DATA_HOME/recall,
// else ~/.local/share/recall, with the spellings before 0.23.0
// ($POLIGN_RECALL_HOME, and a setup saved in ~/.config/polign/recall) still
// honored. See recallhome.Home.
func Home() string { return recallhome.Home() }

// LegacyHome is where setups before 0.23.0 lived, ~/.config/polign/recall.
func LegacyHome() string { return recallhome.LegacyHome() }
