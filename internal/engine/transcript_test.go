package engine

import (
	"strings"
	"testing"
)

func TestTranscript(t *testing.T) {
	got, err := Transcript([]Message{
		{Role: "assistant", Content: "Do you still live in Seattle?"},
		{Role: "User", Content: "No,\n we moved to  Portland."},
	})
	want := "Assistant: Do you still live in Seattle?\nUser: No, we moved to Portland."
	if err != nil || got != want {
		t.Fatalf("Transcript = %q, %v; want %q", got, err, want)
	}
	for _, bad := range [][]Message{
		nil,
		{{Role: "user", Content: " "}},
		{{Role: "system", Content: "be brief"}},
		{{Role: "user", Content: strings.Repeat("x", MaxTranscript)}},
	} {
		if _, err := Transcript(bad); err == nil {
			t.Errorf("Transcript(%.40v) accepted", bad)
		}
	}
}
