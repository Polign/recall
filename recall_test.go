package recall_test

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/Polign/recall"
)

// Exercise the supported import path with a portable audit bundle. This covers
// both forwarded functions and methods on the public aliases.
func TestPublicAuditRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/audit-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var bundle recall.AuditBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Verify(); err != nil {
		t.Fatal(err)
	}
	beliefs, err := bundle.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(beliefs) != 1 || beliefs[0].Value != "Neovim" {
		t.Fatalf("replayed beliefs = %+v", beliefs)
	}

	events := make([]recall.Event, len(bundle.Events))
	for i, event := range bundle.Events {
		events[i], err = recall.DecodeEvent(event.ID, event.Metadata())
		if err != nil {
			t.Fatal(err)
		}
	}
	digest, err := recall.DigestV2(events)
	if err != nil {
		t.Fatal(err)
	}
	if err := recall.VerifyDigest(bundle.Events, digest); err != nil {
		t.Fatalf("metadata round trip changed the event digest: %v", err)
	}
	bundle.Events = events
	if err := bundle.Verify(); err != nil {
		t.Fatalf("metadata round trip changed the audit bundle: %v", err)
	}
	got, err := bundle.Replay()
	if err != nil || !reflect.DeepEqual(got, beliefs) {
		t.Fatalf("metadata round trip changed beliefs: %+v, %v", got, err)
	}

	if _, err := recall.DecodeEvent("invalid", nil); !errors.Is(err, recall.ErrInvalidEvent) {
		t.Fatalf("decode error does not match the public sentinel: %v", err)
	}
	bundle.Digest = "invalid"
	if err := bundle.Verify(); !errors.Is(err, recall.ErrDigestMismatch) {
		t.Fatalf("audit error does not match the public sentinel: %v", err)
	}
}
