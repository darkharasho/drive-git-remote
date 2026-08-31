// Package update checks GitHub for newer releases and replaces the running
// binary in place.
//
// The check is deliberately quiet: it runs at most once an hour, only on an
// interactive terminal, times out fast, and treats every failure as "no news".
// A version check is never worth interrupting real work over.
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/darkharasho/drive-git-remote/internal/config"
)

// Repo is the GitHub repository releases are published to.
const Repo = "darkharasho/drive-git-remote"

// GitHubBase is overridable so tests can point at a local server.
var GitHubBase = "https://github.com"

// Interval is how often the check re-queries GitHub. At one request per hour
// this stays far inside GitHub's anonymous limit of 60 per hour, even with a
// token absent and several repos in play.
const Interval = time.Hour

// requestTimeout bounds the check so a slow network cannot stall a command.
const requestTimeout = 3 * time.Second

// EnvDisable turns the automatic check off entirely.
const EnvDisable = "DRIVE_GIT_NO_UPDATE_CHECK"

// Release identifies a published release.
type Release struct {
	TagName string
}

// assetURL is the download URL for one of the release's files.
func (r Release) assetURL(name string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", GitHubBase, Repo, r.TagName, name)
}

func request(ctx context.Context, method, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// Latest resolves the most recent published release.
//
// This follows github.com's /releases/latest redirect rather than querying
// api.github.com. The API permits only 60 anonymous requests per hour per IP,
// which a shared address or corporate NAT exhausts easily, and it is a
// separate host that restrictive networks sometimes block outright. Resolving
// through github.com uses the same host the downloads come from: if a machine
// can install, it can check.
func Latest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/%s/releases/latest", GitHubBase, Repo)
	res, err := request(ctx, http.MethodHead, url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("looking up the latest release: GitHub returned %s", res.Status)
	}

	// After redirects, the final URL is .../releases/tag/<tag>. A repo with no
	// releases does not redirect, so the marker is simply absent.
	final := res.Request.URL.String()
	_, tag, found := strings.Cut(final, "/releases/tag/")
	if !found || tag == "" || strings.Contains(tag, "/") {
		return nil, fmt.Errorf("no published releases for %s yet", Repo)
	}
	return &Release{TagName: tag}, nil
}

// ParseVersion reads a vX.Y.Z tag. Development builds report a commit sha or
// "dev", which is not comparable, so ok is false for those.
func ParseVersion(s string) ([3]int, bool) {
	var v [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return v, false
	}
	// Ignore any pre-release or build suffix for comparison purposes.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// Newer reports whether latest is a higher version than current. It is false
// whenever either side is not a comparable release version, so development
// builds are never nagged.
func Newer(current, latest string) bool {
	c, okC := ParseVersion(current)
	l, okL := ParseVersion(latest)
	if !okC || !okL {
		return false
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

type cacheFile struct {
	CheckedAt time.Time `json:"checked_at"`
	LatestTag string    `json:"latest_tag"`
}

func cachePath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "update-check.json"), nil
}

func loadCache() (cacheFile, bool) {
	p, err := cachePath()
	if err != nil {
		return cacheFile{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return cacheFile{}, false
	}
	var c cacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		return cacheFile{}, false
	}
	return c, true
}

func saveCache(c cacheFile) {
	p, err := cachePath()
	if err != nil {
		return
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(p, b, 0o600)
}

// Notice returns a short upgrade message, or "" when there is nothing to say.
// It refreshes the cached result at most once per Interval, and reports no
// error ever: a failed check is silence, not noise.
func Notice(ctx context.Context, current string, now time.Time) string {
	if os.Getenv(EnvDisable) != "" {
		return ""
	}
	if _, ok := ParseVersion(current); !ok {
		return "" // development build; nothing meaningful to compare
	}

	c, ok := loadCache()
	if !ok || now.Sub(c.CheckedAt) > Interval {
		fetchCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		rel, err := Latest(fetchCtx)
		if err != nil {
			// Record the attempt so a repeatedly failing check (private repo,
			// offline) does not retry on every single command.
			saveCache(cacheFile{CheckedAt: now, LatestTag: c.LatestTag})
			return ""
		}
		c = cacheFile{CheckedAt: now, LatestTag: rel.TagName}
		saveCache(c)
	}

	if !Newer(current, c.LatestTag) {
		return ""
	}
	return fmt.Sprintf("\ndrive-git %s is available (you have %s). Run `drive-git update` to upgrade.",
		c.LatestTag, current)
}

// assetName is the release archive for the running platform.
func assetName(version string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("drive-git_%s_%s_%s.%s", version, runtime.GOOS, runtime.GOARCH, ext)
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "drive-git.exe"
	}
	return "drive-git"
}

func download(ctx context.Context, url string) ([]byte, error) {
	res, err := request(ctx, http.MethodGet, url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading: GitHub returned %s", res.Status)
	}
	return io.ReadAll(res.Body)
}

// verify checks the archive against the release's checksums.txt. A self-update
// replaces the binary the user runs, so a corrupted or truncated download must
// not be installed.
func verify(sums []byte, name string, archive []byte) error {
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("no checksum listed for %s", name)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch for %s:\n  expected %s\n  got      %s", name, want, got)
	}
	return nil
}

// extract pulls the drive-git binary out of a release archive.
func extract(name string, archive []byte) ([]byte, error) {
	want := binaryName()
	if strings.HasSuffix(name, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) != want {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
		return nil, fmt.Errorf("%s not found in %s", want, name)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(h.Name) == want {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("%s not found in %s", want, name)
}

// Replace swaps the binary at path for new content, atomically. The temp file
// is created alongside the target so the rename cannot cross filesystems, and
// because a rename over a running executable is fine on Unix.
func Replace(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".drive-git-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Result describes what Apply did.
type Result struct {
	From, To string
	Path     string
	NoChange bool
}

// Apply downloads the latest release and replaces the running binary.
func Apply(ctx context.Context, current string, force bool) (*Result, error) {
	rel, err := Latest(ctx)
	if err != nil {
		return nil, err
	}
	res := &Result{From: current, To: rel.TagName}

	if !force && !Newer(current, rel.TagName) {
		if _, ok := ParseVersion(current); !ok {
			return nil, fmt.Errorf("this is a development build (%s), not a release; use --force to install %s anyway",
				current, rel.TagName)
		}
		res.NoChange = true
		return res, nil
	}

	// The running binary may be reached through the git-remote-gdrive symlink;
	// resolve it so the update replaces the real file and leaves the symlink
	// pointing at the new one.
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	res.Path = exe

	// Release archives are named with the full tag, "v" included.
	name := assetName(rel.TagName)
	archive, err := download(ctx, rel.assetURL(name))
	if err != nil {
		return nil, fmt.Errorf("release %s has no build for %s/%s (expected %s)",
			rel.TagName, runtime.GOOS, runtime.GOARCH, name)
	}
	sums, err := download(ctx, rel.assetURL("checksums.txt"))
	if err != nil {
		return nil, fmt.Errorf("release %s has no checksums.txt; refusing to install an unverified binary", rel.TagName)
	}
	if err := verify(sums, name, archive); err != nil {
		return nil, err
	}
	binary, err := extract(name, archive)
	if err != nil {
		return nil, err
	}
	if err := Replace(exe, binary); err != nil {
		return nil, err
	}
	// Refresh the cache so the notice does not linger after a successful update.
	saveCache(cacheFile{CheckedAt: time.Now(), LatestTag: rel.TagName})
	return res, nil
}
