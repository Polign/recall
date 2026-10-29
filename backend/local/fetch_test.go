package local

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// releaseTarGz builds an archive shaped like a polign_db release: documents
// and binaries at the root, nothing nested.
func releaseTarGz(t *testing.T, server []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, mode int64, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	add("LICENSE", 0o644, []byte("polign_db license\n"))
	add("README.md", 0o644, []byte("readme\n"))
	add("nested/polign-server", 0o755, []byte("not this one\n"))
	add(serverBinaryName(), 0o755, server)
	add("polign", 0o755, []byte("cli\n"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// pinRelease points the download at a test server for this platform's
// archive and pins the given checksum. It returns how many downloads ran.
func pinRelease(t *testing.T, archive []byte, checksum string) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	name, _, _ := archiveFor(runtime.GOOS, runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+ServerVersion+"/"+name || r.URL.Query().Get("c") != "recall" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	oldBase, oldSum := downloadBase, serverArchives[runtime.GOOS+"_"+runtime.GOARCH]
	downloadBase = srv.URL
	serverArchives[runtime.GOOS+"_"+runtime.GOARCH] = checksum
	t.Cleanup(func() { downloadBase = oldBase; serverArchives[runtime.GOOS+"_"+runtime.GOARCH] = oldSum })
	home := t.TempDir()
	t.Setenv("RECALL_HOME", home)
	t.Setenv("RECALL_NO_DOWNLOAD", "")
	// Nothing on PATH, so the lookup falls through to the download.
	t.Setenv("PATH", t.TempDir())
	return &hits
}

func TestFetchServerDownloadsOncePinnedByChecksum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake server is a shell script")
	}
	server := []byte("#!/bin/sh\necho -runtime-file\n")
	archive := releaseTarGz(t, server)
	hits := pinRelease(t, archive, sum(archive))

	if _, err := ServerPath(""); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("ServerPath with nothing installed: %v", err)
	}
	var progress bytes.Buffer
	path, err := fetchServer(context.Background(), &progress)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(DownloadedServerDir(), serverBinaryName()); path != want {
		t.Fatalf("installed at %s, want %s", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, server) {
		t.Fatalf("installed binary: %v %q", err, got)
	}
	if info, _ := os.Stat(path); info.Mode()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", info.Mode())
	}
	if lic, err := os.ReadFile(filepath.Join(DownloadedServerDir(), "polign_db-LICENSE")); err != nil || !strings.Contains(string(lic), "polign_db license") {
		t.Fatalf("license beside the binary: %v %q", err, lic)
	}
	if !strings.Contains(progress.String(), "Downloading polign_db "+ServerVersion) {
		t.Fatalf("progress: %q", progress.String())
	}
	if err := checkServer(path); err != nil {
		t.Fatalf("the downloaded server fails the version check: %v", err)
	}
	// The next lookup finds it without a download.
	again, err := ServerPath("")
	if err != nil || again != path {
		t.Fatalf("ServerPath after download: %s %v", again, err)
	}
	if _, err := fetchServer(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("downloaded %d times, want once", hits.Load())
	}
	// No leftover temporaries.
	entries, _ := os.ReadDir(DownloadedServerDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".download-") || strings.HasPrefix(e.Name(), ".extract-") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestFetchServerRefusesAWrongChecksum(t *testing.T) {
	archive := releaseTarGz(t, []byte("tampered\n"))
	pinRelease(t, archive, strings.Repeat("0", 64))
	_, err := fetchServer(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") || !strings.Contains(err.Error(), "nothing was installed") {
		t.Fatalf("wrong checksum: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(DownloadedServerDir(), serverBinaryName())); statErr == nil {
		t.Fatal("a binary with the wrong checksum was installed")
	}
}

func TestFetchServerHonoursTheOptOut(t *testing.T) {
	archive := releaseTarGz(t, []byte("x\n"))
	hits := pinRelease(t, archive, sum(archive))
	t.Setenv("RECALL_NO_DOWNLOAD", "1")
	_, err := fetchServer(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "RECALL_NO_DOWNLOAD") || !strings.Contains(err.Error(), "pip install polign-db") {
		t.Fatalf("opt-out: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("downloaded despite RECALL_NO_DOWNLOAD")
	}
}

func TestExtractServerFromZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"LICENSE": "lic\n", "nested/" + serverBinaryName(): "no\n", serverBinaryName(): "server\n", "polign.exe": "cli\n"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "r.zip")
	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractServer(archive, "zip", dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, serverBinaryName()))
	if string(got) != "server\n" {
		t.Fatalf("extracted %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "polign.exe")); err == nil {
		t.Fatal("extracted a binary it should have left alone")
	}
	if _, err := os.Stat(filepath.Join(dir, "polign_db-LICENSE")); err != nil {
		t.Fatal("license not extracted")
	}
}

func TestArchiveFor(t *testing.T) {
	for _, c := range []struct{ goos, goarch, name, format string }{
		{"darwin", "arm64", "polign_db_darwin_arm64.tar.gz", "tar.gz"},
		{"linux", "amd64", "polign_db_linux_amd64.tar.gz", "tar.gz"},
		{"windows", "amd64", "polign_db_windows_amd64.zip", "zip"},
	} {
		name, format, ok := archiveFor(c.goos, c.goarch)
		if !ok || name != c.name || format != c.format {
			t.Errorf("%s/%s: %s %s %v", c.goos, c.goarch, name, format, ok)
		}
	}
	if _, _, ok := archiveFor("plan9", "amd64"); ok {
		t.Error("plan9 has a release?")
	}
	for key, sum := range serverArchives {
		if len(sum) != 64 {
			t.Errorf("%s: checksum %q is not a SHA-256", key, sum)
		}
	}
}
