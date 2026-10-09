package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
)

// Keep the guide in the binary so it describes the installed release and
// remains available before credentials, network access, or Recall setup exist.
//
//go:embed guides/recall.md
var recallGuide string

func cmdSkill(args []string, out io.Writer) error {
	command, title, guide := "recall skill", "Recall", recallGuide
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: %s\nPrint the embedded %s agent guide as Markdown. No network or setup required.\n", command, title)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: %s (no arguments)", command)
	}
	_, err := fmt.Fprintf(out, "# %s agent guide\n\nBundled with recall %s.\n\n%s", title, version, guide)
	return err
}
