// Package gitx is a thin wrapper around the git CLI. We shell out rather than
// reimplement anything: bundles, rev-list and ref plumbing are all first-class
// in git itself.
package gitx

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Repo is a local git repository working directory (or a bare repo dir).
type Repo struct {
	Dir string
}

// Run executes git in the repo and returns trimmed stdout.
func (r Repo) Run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// RunOK reports whether the command succeeded, discarding output.
func (r Repo) RunOK(args ...string) bool {
	_, err := r.Run(args...)
	return err == nil
}

// Refs lists refs under the given prefixes as a name->sha map, using the full
// ref name (e.g. "refs/heads/main") as the key.
func (r Repo) Refs(prefixes ...string) (map[string]string, error) {
	args := append([]string{"for-each-ref", "--format=%(refname) %(objectname)"}, prefixes...)
	out, err := r.Run(args...)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, sha, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		refs[name] = sha
	}
	return refs, nil
}

// SortedNames returns the ref names of a set in stable order.
func SortedNames(refs map[string]string) []string {
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SortedSHAs returns the unique object IDs of a ref set in stable order.
func SortedSHAs(refs map[string]string) []string {
	seen := map[string]bool{}
	var shas []string
	for _, sha := range refs {
		if !seen[sha] {
			seen[sha] = true
			shas = append(shas, sha)
		}
	}
	sort.Strings(shas)
	return shas
}

// IsRepo reports whether dir is inside a git repository.
func IsRepo(dir string) bool {
	return Repo{Dir: dir}.RunOK("rev-parse", "--git-dir")
}

// Toplevel returns the root of the working tree containing dir.
func Toplevel(dir string) (string, error) {
	return Repo{Dir: dir}.Run("rev-parse", "--show-toplevel")
}

// CurrentBranch returns the checked-out branch name, or "" when detached or
// on an unborn branch.
func (r Repo) CurrentBranch() (string, error) {
	out, err := r.Run("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", nil
	}
	return out, nil
}

// HasCommits reports whether HEAD resolves to a commit.
func (r Repo) HasCommits() bool {
	return r.RunOK("rev-parse", "--verify", "--quiet", "HEAD")
}

// IsClean reports whether the working tree has no staged or unstaged changes.
func (r Repo) IsClean() (bool, error) {
	out, err := r.Run("status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// SetRef points a ref at a sha.
func (r Repo) SetRef(name, sha string) error {
	_, err := r.Run("update-ref", name, sha)
	return err
}

// DeleteRef removes a ref.
func (r Repo) DeleteRef(name string) error {
	_, err := r.Run("update-ref", "-d", name)
	return err
}

// Config reads a git config value, returning "" when unset.
func (r Repo) Config(key string) string {
	out, err := r.Run("config", "--local", "--get", key)
	if err != nil {
		return ""
	}
	return out
}

// SetConfig writes a local git config value.
func (r Repo) SetConfig(key, value string) error {
	_, err := r.Run("config", "--local", key, value)
	return err
}
