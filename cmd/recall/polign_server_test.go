package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Recall reaches polign_db only through its HTTP API, so these tests run a
// real polign-server: $POLIGN_SERVER_BIN, or polign-server on PATH (the
// polign_db wheel, Homebrew cask and release archives all install one).
// Without it the tests skip, unless RECALL_REQUIRE_POLIGN is set, as CI does,
// so a missing server can never pass for green.

func polignServerBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("POLIGN_SERVER_BIN"); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("polign-server")
	if err != nil {
		if os.Getenv("RECALL_REQUIRE_POLIGN") != "" {
			t.Fatal("polign-server not found: set POLIGN_SERVER_BIN or put it on PATH")
		}
		t.Skip("polign-server not found; set POLIGN_SERVER_BIN to run the integration tests")
	}
	return bin
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// serverOptions shape one test server.
type serverOptions struct {
	dir       string // store directory; a fresh one when empty
	coldFirst bool   // serve from object-store segments, as -store does by default
}

// polignServer starts polign-server on a store of its own and returns its
// base URL. The server stops when the test ends.
func polignServer(t *testing.T) string {
	t.Helper()
	return startPolign(t, serverOptions{})
}

func startPolign(t *testing.T, o serverOptions) string {
	t.Helper()
	url, stop := startPolignStoppable(t, o)
	t.Cleanup(stop)
	return url
}

// startPolignStoppable starts polign-server and returns a stop function that
// shuts it down gracefully and waits, so a test can restart on the same store.
func startPolignStoppable(t *testing.T, o serverOptions) (string, func()) {
	t.Helper()
	bin := polignServerBin(t)
	if o.dir == "" {
		o.dir = t.TempDir()
	}
	addr := freeAddr(t)
	cmd := exec.Command(bin,
		"-store", "fs:"+filepath.Join(o.dir, "data"),
		"-http", addr,
		"-grpc", freeAddr(t),
		fmt.Sprintf("-cold-first=%t", o.coldFirst),
		"-telemetry=false",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		})
	}
	t.Cleanup(stop)
	url := "http://" + addr
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return url, stop
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("polign-server at %s did not become healthy", url)
	return "", stop
}
