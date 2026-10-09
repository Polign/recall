package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// apiError carries the status code so a caller can react to one in
// particular, while its message stays the server's own.
func TestAPIErrorCarriesStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		io.WriteString(w, `{"error":"collections API not enabled on this server"}`)
	}))
	defer ts.Close()

	c := &api{base: ts.URL}
	err := c.do("GET", "/v1/collections", nil, nil)

	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("error is %T, want *apiError", err)
	}
	if ae.Code != http.StatusNotImplemented {
		t.Errorf("Code = %d, want %d", ae.Code, http.StatusNotImplemented)
	}
	// The rendered text is unchanged, so the CLI's own output is untouched.
	if got, want := err.Error(), "501 Not Implemented: collections API not enabled on this server"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// A non-JSON error body still becomes an apiError, with the body as the
// message rather than a silently dropped status.
func TestAPIErrorFromPlainBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream exploded", http.StatusBadGateway)
	}))
	defer ts.Close()

	c := &api{base: ts.URL}
	err := c.do("GET", "/v1/collections", nil, nil)

	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("error is %T, want *apiError", err)
	}
	if ae.Code != http.StatusBadGateway || ae.Message != "upstream exploded" {
		t.Errorf("got %d %q, want 502 %q", ae.Code, ae.Message, "upstream exploded")
	}
}
