package local

// polign-server on demand. pip and Homebrew install polign-server beside
// recall, but `go install` and a copied binary do not. Rather than refuse to
// run a local database, the backend downloads the one release of polign_db it
// was tested with, checks it against a SHA-256 pinned here, keeps only
// polign-server (and its license) under Recall's home, and runs that. The
// download happens once per machine. RECALL_NO_DOWNLOAD=1 turns it off, for
// hosts that must not fetch executables; the error then says how to install
// polign_db by hand.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Polign/recall/internal/recallhome"
	"github.com/Polign/recall/internal/recallsetup"
)

// ServerVersion is the polign_db release the local backend downloads when no
// polign-server is installed. Bump it, and serverArchives with it, from the
// release's checksums.txt (dl.polign.com/<version>/checksums.txt).
const ServerVersion = "v0.15.0"

// serverArchives pins each release archive to its SHA-256, so a download is
// exactly the build Recall was tested with, whatever answers the URL.
var serverArchives = map[string]string{
	"darwin_amd64":  "978c09af711dedb61349e7114ed05659a702dddb80ddbf7a22d0a426b61286ef",
	"darwin_arm64":  "1d5a1e6cd538bb942c993b50d8a0f9f20d7d06a56606236f38c37bda1555977a",
	"linux_amd64":   "4e836ad3bac16e1903d2d2a37126e5ad51989784be87f71b36aa5b2c27b1f955",
	"linux_arm64":   "3fdacba8628752cc86b8689ff3bf41e05e216e3fb7e970cc0ee2323704707714",
	"windows_amd64": "4884da1eedb8f29e58fd288f1aab3cdff0446964d2cd2463d4da453879fa2da6",
	"windows_arm64": "61ae5ee3e228f981e92c438b5f7ce3bb74a19c01a6de2737fc1c42b93cfb66bd",
}

// downloadBase serves the release archives; tests point it elsewhere.
var downloadBase = "https://dl.polign.com"

// errServerMissing is ServerPath's answer when no polign-server is installed
// anywhere it looks.
var errServerMissing = errors.New("local: polign-server is not installed")

// installHint says how to get polign-server without the download.
const installHint = "install polign_db with `pip install polign-db` or `brew install polign/tap/polign`, or name a database with -backend and -url"

func serverBinaryName() string {
	if runtime.GOOS == "windows" {
		return "polign-server.exe"
	}
	return "polign-server"
}

// DownloadedServerDir is where a downloaded polign-server lives: bin/ under
// Recall's home ($RECALL_HOME, then ~/.local/share/recall). It is "" when the
// user's home directory is unknown.
func DownloadedServerDir() string {
	home := recallhome.Home()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "bin")
}

// archiveFor names the release archive for this platform and its format.
func archiveFor(goos, goarch string) (name, format string, ok bool) {
	key := goos + "_" + goarch
	if _, ok := serverArchives[key]; !ok {
		return "", "", false
	}
	if goos == "windows" {
		return "polign_db_" + key + ".zip", "zip", true
	}
	return "polign_db_" + key + ".tar.gz", "tar.gz", true
}

// fetchServer downloads polign_db ServerVersion for this machine into
// DownloadedServerDir, verifies it, extracts polign-server and the license,
// and returns the executable's path. progress hears what is happening.
func fetchServer(ctx context.Context, progress io.Writer) (string, error) {
	if os.Getenv("RECALL_NO_DOWNLOAD") == "1" {
		return "", fmt.Errorf("%w and RECALL_NO_DOWNLOAD is set, so Recall will not download it; %s", errServerMissing, installHint)
	}
	key := runtime.GOOS + "_" + runtime.GOARCH
	name, format, ok := archiveFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return "", fmt.Errorf("%w, and polign_db %s has no release for %s/%s; %s", errServerMissing, ServerVersion, runtime.GOOS, runtime.GOARCH, installHint)
	}
	dir := DownloadedServerDir()
	if dir == "" {
		return "", fmt.Errorf("%w, and the user's home directory is unknown, so there is nowhere to download it; set RECALL_HOME, or %s", errServerMissing, installHint)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	// One download per machine, even when several processes start at once:
	// the second waits and finds the first one's binary.
	lock, err := recallsetup.LockWait(filepath.Join(dir, ".lock"), 15*time.Minute)
	if err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	defer lock.Close()
	dest := filepath.Join(dir, serverBinaryName())
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	url := downloadBase + "/" + ServerVersion + "/" + name + "?c=recall"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Do(req) // #nosec G704 -- The URL is a pinned release of Polign's own software, not user input.
	if err != nil {
		return "", fmt.Errorf("local: download polign-server: %w; %s", err, installHint)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("local: download polign-server: %s: %s; %s", url, resp.Status, installHint)
	}
	if progress != nil {
		size := ""
		if resp.ContentLength > 0 {
			size = fmt.Sprintf(" (%d MB)", resp.ContentLength>>20)
		}
		fmt.Fprintf(progress, "Downloading polign_db %s%s once, for the local database: polign-server goes in %s with its license beside it.\n", ServerVersion, size, dir)
	}
	archive, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	defer os.Remove(archive.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(archive, h), resp.Body); err != nil {
		archive.Close()
		return "", fmt.Errorf("local: download polign-server: %w", err)
	}
	if err := archive.Close(); err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	if got, want := hex.EncodeToString(h.Sum(nil)), serverArchives[key]; got != want {
		return "", fmt.Errorf("local: download polign-server: %s has checksum %s, want %s; nothing was installed. Try again, or %s", name, got, want, installHint)
	}
	if err := extractServer(archive.Name(), format, dir); err != nil {
		return "", fmt.Errorf("local: download polign-server: %w", err)
	}
	return dest, nil
}

// extractServer takes polign-server and LICENSE out of a verified release
// archive into dir, writing each whole before it appears under its name.
func extractServer(archive, format, dir string) error {
	want := map[string]string{serverBinaryName(): serverBinaryName(), "LICENSE": "polign_db-LICENSE"}
	found := 0
	write := func(name string, r io.Reader) error {
		// Only entries at the archive root count; the release puts the
		// binaries there, and nothing nested can stand in for them.
		name = strings.TrimPrefix(name, "./")
		target, ok := want[name]
		if !ok {
			return nil
		}
		mode := os.FileMode(0o644)
		if target == serverBinaryName() {
			mode = 0o755
		}
		tmp, err := os.CreateTemp(dir, ".extract-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := io.Copy(tmp, r); err != nil { //nolint:gosec // G110: the archive's checksum was verified against a pinned release
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Chmod(tmp.Name(), mode); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), filepath.Join(dir, target)); err != nil {
			return err
		}
		found++
		return nil
	}
	switch format {
	case "tar.gz":
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if hdr.Typeflag != tar.TypeReg {
				continue
			}
			if err := write(hdr.Name, tr); err != nil {
				return err
			}
		}
	case "zip":
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = write(f.Name, rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown archive format %q", format)
	}
	if found < 1 {
		return fmt.Errorf("%s holds no %s", filepath.Base(archive), serverBinaryName())
	}
	if _, err := os.Stat(filepath.Join(dir, serverBinaryName())); err != nil {
		return fmt.Errorf("%s holds no %s", filepath.Base(archive), serverBinaryName())
	}
	return nil
}
