// recall-audit verifies a JSON audit bundle from stdin and prints its replayed
// beliefs. It needs no database, network connection, or embedding service.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/Polign/recall"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out io.Writer) error {
	flags := flag.NewFlagSet("recall-audit", flag.ContinueOnError)
	trusted := flags.String("digest", "", "expected bundle digest obtained through a trusted channel")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: recall-audit [-digest expected] < bundle.json")
	}
	const maxBytes = 64 << 20
	raw, err := io.ReadAll(io.LimitReader(in, maxBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxBytes {
		return fmt.Errorf("audit input exceeds 64 MiB")
	}
	if !utf8.Valid(raw) {
		return fmt.Errorf("audit input must be UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b recall.AuditBundle
	if err := dec.Decode(&b); err != nil {
		return fmt.Errorf("decode audit: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("audit input must contain exactly one JSON bundle")
	}
	if *trusted != "" && b.Digest != *trusted {
		return recall.ErrDigestMismatch
	}
	beliefs, err := b.Replay()
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(beliefs)
}
