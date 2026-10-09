package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillNeedsNoRecallConfiguration(t *testing.T) {
	// An unreadable-as-a-directory config path catches accidental entry into
	// Recall's setup/config loading path before the guide is printed.
	config := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(config, []byte("leave this unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POLIGN_RECALL_HOME", config)
	t.Setenv("POLIGN_URL", "not-a-valid-server-url")
	t.Setenv("POLIGN_API_KEY", "private-test-key-must-not-be-printed")
	t.Setenv("POLIGN_PREDICATES", filepath.Join(config, "missing.json"))
	var out bytes.Buffer
	if err := cmdSkill(nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "# ") || !strings.Contains(out.String(), "Bundled with recall "+version+".") {
		t.Fatalf("missing Markdown title or installed version: %q", out.String())
	}
	if strings.Contains(out.String(), "private-test-key-must-not-be-printed") || strings.Contains(out.String(), config) {
		t.Fatal("guide exposed runtime configuration")
	}
	data, err := os.ReadFile(config)
	if err != nil || string(data) != "leave this unchanged" {
		t.Fatalf("configuration was changed: %q, %v", data, err)
	}
}

func TestSkillArguments(t *testing.T) {
	for _, args := range [][]string{{"extra"}, {"-url", "http://example.invalid"}, {"--write"}} {
		var out bytes.Buffer
		if err := cmdSkill(args, &out); err == nil {
			t.Errorf("accepted %v", args)
		}
		if strings.Contains(out.String(), "Bundled with") {
			t.Error("printed a guide after invalid arguments")
		}
	}
	for _, arg := range []string{"-h", "--help"} {
		var out bytes.Buffer
		if err := cmdSkill([]string{arg}, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Usage:") {
			t.Fatal("missing help")
		}
	}
}

type failedSkillWriter struct{}

func (failedSkillWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestSkillReportsOutputFailure(t *testing.T) {
	if err := cmdSkill(nil, failedSkillWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("error = %v, want closed pipe", err)
	}
}
