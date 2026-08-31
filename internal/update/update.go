// Package update checks GitHub for newer releases and replaces the running
// binary in place.
//
// The check is deliberately quiet: it runs at most once a day, only on an
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
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/darkharasho/drive-git-remote/internal/config"
)

// Repo is the GitHub repository releases are published to.
const Repo = "darkharasho/drive-git-remote"

// APIBase is overridable so tests can point at a local server.
var APIBase = "https://api.github.com"

// Interval is how often the background check re-queries GitHub.
const Interval = 24 * time.Hour

// requestTimeout bounds the check so a slow network cannot stall a command.
const requestTimeout = 3 * time.Second

// EnvDisable turns the automatic check off entirely.
const EnvDisable = "DRIVE_GIT_NO_UPDATE_CHECK"

// Asset is a release artifact. URL is the API URL rather than the browser
// one, because that form works for private repositories too.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Release is the subset of GitHub's release payload we use.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// token returns a GitHub token if one is available. The repository is public,
// so this is not required — it raises the anonymous 60-requests-per-hour API
// limit, and keeps the tool working if the repo is ever made private.
func token() string {
	for _, key := range []string{"DRIVE_GIT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	if _, err := exec.LookPath("gh"); err == nil {
		out, err := exec.Command("gh", "auth", "token").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}

func request(ctx context.Context, method, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if t := token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	return http.DefaultClient.Do(req)
}

// Latest fetches the most recent published release.
func Latest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", APIBase, Repo)
	res, err := request(ctx, http.MethodGet, url, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// Also what a private repo looks like without a token, but for a
		// public one this simply means nothing has been tagged yet.
		return nil, fmt.Errorf("no published releases for %s yet", Repo)
	default:
		return nil, fmt.Errorf("GitHub returned %s", res.Status)
	}
	var r Release
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, err
	}
	if r.TagName == "" {
		return nil, fmt.Errorf("release has no tag")
	}
	return &r, nil
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
	res, err := request(ctx, http.MethodGet, url, "application/octet-stream")
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
	var archiveURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case name:
			archiveURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		}
	}
	if archiveURL == "" {
		return nil, fmt.Errorf("release %s has no build for %s/%s (expected %s)",
			rel.TagName, runtime.GOOS, runtime.GOARCH, name)
	}
	if sumsURL == "" {
		return nil, fmt.Errorf("release %s has no checksums.txt; refusing to install an unverified binary", rel.TagName)
	}

	archive, err := download(ctx, archiveURL)
	if err != nil {
		return nil, err
	}
	sums, err := download(ctx, sumsURL)
	if err != nil {
		return nil, err
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
