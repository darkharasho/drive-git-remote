package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.0", "v1.1.0", true},
		{"v1.0.0", "v2.0.0", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.2.2", false},
		{"v2.0.0", "v1.9.9", false},
		{"1.0.0", "v1.0.1", true},
		// Ordering must be numeric, not lexicographic.
		{"v1.9.0", "v1.10.0", true},
		{"v1.10.0", "v1.9.0", false},
		// Development builds are never comparable, so never nagged.
		{"dev", "v1.0.0", false},
		{"c23fcbe", "v1.0.0", false},
		{"v1.0.0", "not-a-version", false},
		// Pre-release suffixes compare on the numeric part.
		{"v1.0.0-rc1", "v1.0.1", true},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

// buildArchive produces a release tarball holding a fake binary.
func buildArchive(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{binaryName(), content},
		{"README.md", "docs"},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name, Mode: 0o755, Size: int64(len(f.body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// releaseServer stands in for the GitHub API.
func releaseServer(t *testing.T, tag string, archive, sums []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[
			{"name":%q,"url":"%s/asset/archive"},
			{"name":"checksums.txt","url":"%s/asset/sums"}]}`,
			tag, assetName(tag), srv.URL, srv.URL)
	})
	mux.HandleFunc("/asset/archive", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/asset/sums", func(w http.ResponseWriter, r *http.Request) { w.Write(sums) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// sandbox points config and the API at throwaway locations.
func sandbox(t *testing.T, apiBase string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("DRIVE_GIT_NO_UPDATE_CHECK", "")
	// Do not let a real gh credential or CI token leak into the test.
	t.Setenv("DRIVE_GIT_GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	old := APIBase
	APIBase = apiBase
	t.Cleanup(func() { APIBase = old })
}

func TestApplyReplacesBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("archive layout differs on windows")
	}
	tag := "v9.9.9"
	archive := buildArchive(t, "new binary")
	sums := []byte(sha256hex(archive) + "  " + assetName(tag) + "\n")
	srv := releaseServer(t, tag, archive, sums)
	sandbox(t, srv.URL)

	// Apply replaces the running executable, so point that at a temp copy.
	dir := t.TempDir()
	fake := filepath.Join(dir, "drive-git")
	if err := os.WriteFile(fake, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	content, err := extract(assetName(tag), archive)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if err := Replace(fake, content); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := os.ReadFile(fake)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Fatalf("binary not replaced, got %q", got)
	}
	info, err := os.Stat(fake)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("replaced binary is not executable: %v", info.Mode())
	}
}

// TestReplacePreservesSymlink is the property install-helper depends on: the
// git-remote-gdrive symlink must still resolve after an update.
func TestReplacePreservesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks are POSIX-only here")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "drive-git")
	link := filepath.Join(dir, "git-remote-gdrive")
	if err := os.WriteFile(real, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := Replace(real, []byte("new")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("symlink broken after update: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("symlink resolves to stale content %q", got)
	}
}

func TestVerifyRejectsTamperedArchive(t *testing.T) {
	tag := "v1.0.0"
	archive := buildArchive(t, "good")
	name := assetName(tag)
	sums := []byte(sha256hex(archive) + "  " + name + "\n")

	if err := verify(sums, name, archive); err != nil {
		t.Fatalf("valid archive rejected: %v", err)
	}
	if err := verify(sums, name, []byte("tampered")); err == nil {
		t.Fatal("expected a checksum mismatch to be caught")
	}
	if err := verify([]byte("deadbeef  other-file\n"), name, archive); err == nil {
		t.Fatal("expected a missing checksum entry to be an error")
	}
}

func TestApplyRefusesWithoutChecksums(t *testing.T) {
	tag := "v9.9.9"
	archive := buildArchive(t, "x")
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"url":"%s/a"}]}`, tag, assetName(tag), srv.URL)
	})
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	srv = httptest.NewServer(mux)
	defer srv.Close()
	sandbox(t, srv.URL)

	_, err := Apply(context.Background(), "v0.0.1", false)
	if err == nil || !strings.Contains(err.Error(), "checksums.txt") {
		t.Fatalf("expected a refusal to install unverified, got %v", err)
	}
}

func TestNoticeCachesAndReportsOnce(t *testing.T) {
	tag := "v9.9.9"
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[]}`, tag)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sandbox(t, srv.URL)

	now := time.Now()
	msg := Notice(context.Background(), "v1.0.0", now)
	if !strings.Contains(msg, tag) {
		t.Fatalf("expected an upgrade notice mentioning %s, got %q", tag, msg)
	}
	if hits != 1 {
		t.Fatalf("expected one API call, got %d", hits)
	}

	// Within the interval the cache answers, without touching the network.
	// Expressed relative to Interval so tuning it does not silently turn this
	// into a boundary test.
	if msg := Notice(context.Background(), "v1.0.0", now.Add(Interval/2)); msg == "" {
		t.Fatal("expected the cached notice to still report")
	}
	if hits != 1 {
		t.Fatalf("expected the cache to be reused, saw %d API calls", hits)
	}

	// Exactly at the interval still counts as fresh; the check is strict.
	if Notice(context.Background(), "v1.0.0", now.Add(Interval)); hits != 1 {
		t.Fatalf("expected no refresh exactly at the interval, saw %d API calls", hits)
	}

	// Past the interval it re-checks.
	if Notice(context.Background(), "v1.0.0", now.Add(Interval+time.Minute)) == "" {
		t.Fatal("expected a notice after the cache expired")
	}
	if hits != 2 {
		t.Fatalf("expected a refresh after the interval, saw %d API calls", hits)
	}

	// A current version gets no notice at all.
	if msg := Notice(context.Background(), tag, now.Add(Interval/2)); msg != "" {
		t.Fatalf("expected silence when up to date, got %q", msg)
	}
}

func TestNoticeIsSilentOnFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sandbox(t, srv.URL)

	if msg := Notice(context.Background(), "v1.0.0", time.Now()); msg != "" {
		t.Fatalf("a failed check must be silent, got %q", msg)
	}
	// The failed attempt is still recorded, so we do not retry every command.
	if c, ok := loadCache(); !ok || c.CheckedAt.IsZero() {
		t.Fatal("expected the failed attempt to be cached")
	}
}

func TestNoticeRespectsOptOut(t *testing.T) {
	srv := releaseServer(t, "v9.9.9", nil, nil)
	sandbox(t, srv.URL)
	t.Setenv(EnvDisable, "1")
	if msg := Notice(context.Background(), "v1.0.0", time.Now()); msg != "" {
		t.Fatalf("expected opt-out to silence the check, got %q", msg)
	}
}

func TestNoticeSkipsDevelopmentBuilds(t *testing.T) {
	srv := releaseServer(t, "v9.9.9", nil, nil)
	sandbox(t, srv.URL)
	for _, v := range []string{"dev", "c23fcbe", ""} {
		if msg := Notice(context.Background(), v, time.Now()); msg != "" {
			t.Fatalf("expected silence for development build %q, got %q", v, msg)
		}
	}
}
