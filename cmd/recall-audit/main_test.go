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
		"unknown version": strings.Replace(string(raw), "recall-fold-v2", "recall-fold-v3", 1),
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

// memDB is the smallest VectorDB that can produce an audit bundle: exact
// listing only, which is all ExportAudit reads.
type memDB struct {
	ids  []string
	rows map[string]map[string]any
}

func (m *memDB) Put(_, id string, _ []float32, md map[string]any) error {
	if m.rows == nil {
		m.rows = map[string]map[string]any{}
	}
	if _, ok := m.rows[id]; !ok {
		m.ids = append(m.ids, id)
	}
	m.rows[id] = md
	return nil
}

func (m *memDB) List(_ string, filter map[string]any, limit int) ([]recall.StoredVector, int, error) {
	var out []recall.StoredVector
	for _, id := range m.ids {
		match := true
		for k, v := range filter {
			match = match && m.rows[id][k] == v
		}
		if match {
			out = append(out, recall.StoredVector{ID: id, Metadata: m.rows[id]})
		}
	}
	total := len(out)
	return out[:min(limit, total)], total, nil
}

func (m *memDB) Search(string, []float32, int, map[string]any) ([]recall.Hit, error) {
	return nil, nil
}

func TestNotesFlagPrintsOnlyNotes(t *testing.T) {
	s := recall.NewStore(&memDB{}, "m", recall.DefaultRegistry(), func(string) []float32 { return []float32{1} })
	if _, err := s.Remember("preference", "user", "prefers_editor", "neovim", 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember("fact", "user", recall.NotePredicate, "I use fish as my shell.", 0.5, "agent_inferred"); err != nil {
		t.Fatal(err)
	}
	b, err := s.ExportAudit(recall.AuditRequest{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"-notes"}, bytes.NewReader(raw), &out); err != nil {
		t.Fatal(err)
	}
	var beliefs []recall.Belief
	if err := json.Unmarshal(out.Bytes(), &beliefs); err != nil {
		t.Fatal(err)
	}
	if len(beliefs) != 1 || beliefs[0].Value != "I use fish as my shell." {
		t.Fatalf("notes = %+v", beliefs)
	}
}
