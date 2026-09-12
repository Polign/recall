package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Polign/recall"
)

func TestOfflineVerificationAndReplay(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/audit-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var b recall.AuditBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"-digest", b.Digest}, bytes.NewReader(raw), &out); err != nil {
		t.Fatal(err)
	}
	var beliefs []recall.Belief
	if err := json.Unmarshal(out.Bytes(), &beliefs); err != nil {
		t.Fatal(err)
	}
	if len(beliefs) != 1 || beliefs[0].Value != "Neovim" {
		t.Fatalf("replayed = %+v", beliefs)
	}
	for name, input := range map[string]string{
		"case tampering":  strings.Replace(string(raw), "Neovim", "neovim", 1),
		"unknown version": strings.Replace(string(raw), "recall-fold-v1", "recall-fold-v2", 1),
		"unknown field":   strings.Replace(string(raw), "\"as_of\"", "\"not_as_of\"", 1),
		"trailing JSON":   string(raw) + "{}",
		"invalid JSON":    "{",
		"invalid UTF-8":   strings.Replace(string(raw), "Neovim", "\xff", 1),
	} {
		t.Run(name, func(t *testing.T) {
			out.Reset()
			if err := run(nil, strings.NewReader(input), &out); err == nil || out.Len() != 0 {
				t.Fatalf("invalid input produced output: %s, %v", &out, err)
			}
		})
	}
	out.Reset()
	if err := run([]string{"-digest", "wrong"}, bytes.NewReader(raw), &out); err == nil || out.Len() != 0 {
		t.Fatal("untrusted digest accepted")
	}
}
