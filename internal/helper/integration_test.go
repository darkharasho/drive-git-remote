package helper_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/darkharasho/drive-git-remote/internal/local"
)

// env is a fully wired sandbox: a built binary installed as both drive-git and
// git-remote-gdrive, on PATH, backed by a local directory instead of Drive.
type env struct {
	t       *testing.T
	bin     string
	store   string
	homeDir string
}

func setup(t *testing.T) *env {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink-based helper install is POSIX-only in this test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	base := t.TempDir()
	binDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	driveGit := filepath.Join(binDir, "drive-git")

	build := exec.Command("go", "build", "-o", driveGit, "github.com/darkharasho/drive-git-remote/cmd/drive-git")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building drive-git: %v", err)
	}
	// The same binary serves the helper protocol when invoked under this name.
	if err := os.Symlink(driveGit, filepath.Join(binDir, "git-remote-gdrive")); err != nil {
		t.Fatal(err)
	}

	e := &env{
		t:       t,
		bin:     driveGit,
		store:   filepath.Join(base, "drive"),
		homeDir: filepath.Join(base, "home"),
	}
	if err := os.MkdirAll(e.homeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(local.EnvRoot, e.store)
	// Keep config and any generated key inside the sandbox.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(e.homeDir, ".config"))
	t.Setenv("HOME", e.homeDir)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(e.homeDir, "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	return e
}

// run executes a command in dir, failing the test on error.
func (e *env) run(dir, name string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("%s %s (in %s): %v\n%s", name, strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// tryRun is run for commands that are expected to fail.
func (e *env) tryRun(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// newRepo creates a git repo with one commit.
func (e *env) newRepo(name string) string {
	e.t.Helper()
	dir := filepath.Join(e.t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.run(dir, "git", "init", "--quiet", "--initial-branch=main")
	e.run(dir, "git", "config", "user.email", "test@example.com")
	e.run(dir, "git", "config", "user.name", "Test")
	e.commit(dir, "deploy.sh", "echo one\n")
	return dir
}

func (e *env) commit(dir, file, body string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.run(dir, "git", "add", file)
	e.run(dir, "git", "commit", "--quiet", "-m", "write "+file)
}

func (e *env) readFile(dir, name string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		e.t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// TestGitCloneAndPushThroughHelper is the end-to-end path: plain git commands,
// no wrapper, talking to the store through git-remote-gdrive.
func TestGitCloneAndPushThroughHelper(t *testing.T) {
	e := setup(t)
	src := e.newRepo("scripts")
	e.run(src, e.bin, "init", "--name", "scripts")

	// Clone with plain git, via the gdrive:// URL.
	work := t.TempDir()
	e.run(work, "git", "clone", "gdrive://scripts", "clone")
	clone := filepath.Join(work, "clone")

	if got := e.readFile(clone, "deploy.sh"); got != "echo one\n" {
		t.Fatalf("clone content mismatch: %q", got)
	}
	if branch := e.run(clone, "git", "rev-parse", "--abbrev-ref", "HEAD"); branch != "main" {
		t.Fatalf("expected main checked out, got %q", branch)
	}

	// Push with plain git.
	e.run(clone, "git", "config", "user.email", "test@example.com")
	e.run(clone, "git", "config", "user.name", "Test")
	e.commit(clone, "extra.sh", "echo two\n")
	e.run(clone, "git", "push", "origin", "main")

	// And pull it back into the original repo with plain git.
	e.run(src, "git", "pull", "--ff-only", "gdrive", "main")
	if got := e.readFile(src, "extra.sh"); got != "echo two\n" {
		t.Fatalf("pushed content did not round trip: %q", got)
	}

	// Encryption is opt-in, so these links are plain bundles.
	for _, name := range e.storedLinks("scripts") {
		if strings.HasSuffix(name, ".age") {
			t.Fatalf("expected unencrypted links by default, found %s", name)
		}
	}
}

// storedLinks lists the link filenames a repo has in the store.
func (e *env) storedLinks(repo string) []string {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.store, "drive-git-remote", repo))
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".bundle") || strings.Contains(entry.Name(), ".refs") {
			out = append(out, entry.Name())
		}
	}
	if len(out) == 0 {
		e.t.Fatalf("no links found for %s", repo)
	}
	return out
}

// TestHelperRejectsNonFastForward checks the push path refuses to clobber
// remote history unless forced.
func TestHelperRejectsNonFastForward(t *testing.T) {
	e := setup(t)
	src := e.newRepo("scripts")
	e.run(src, e.bin, "init", "--name", "scripts")

	work := t.TempDir()
	e.run(work, "git", "clone", "gdrive://scripts", "clone")
	clone := filepath.Join(work, "clone")
	e.run(clone, "git", "config", "user.email", "test@example.com")
	e.run(clone, "git", "config", "user.name", "Test")

	// The clone advances the remote.
	e.commit(clone, "from-clone.sh", "echo clone\n")
	e.run(clone, "git", "push", "origin", "main")

	// The source, still at the old tip, must be rejected.
	e.commit(src, "from-src.sh", "echo src\n")
	out, err := e.tryRun(src, "git", "push", "gdrive", "main")
	if err == nil {
		t.Fatalf("expected the diverged push to be rejected, got:\n%s", out)
	}
	if !strings.Contains(out, "non-fast-forward") && !strings.Contains(out, "changes you do not have") {
		t.Fatalf("expected a divergence message, got:\n%s", out)
	}

	// After integrating, the same push succeeds.
	e.run(src, "git", "fetch", "gdrive")
	e.run(src, "git", "merge", "--no-edit", "--quiet", "gdrive/main")
	e.run(src, "git", "push", "gdrive", "main")
}

// TestHelperRenameAndDeleteRefspecs covers the refspec forms that have no
// matching local ref name, which is why bundles carry mirror-namespace refs.
func TestHelperRenameAndDeleteRefspecs(t *testing.T) {
	e := setup(t)
	src := e.newRepo("scripts")
	e.run(src, e.bin, "init", "--name", "scripts")

	// Push local main to a differently named remote branch.
	e.run(src, "git", "push", "gdrive", "main:trunk")
	refs := e.run(src, "git", "ls-remote", "gdrive")
	if !strings.Contains(refs, "refs/heads/trunk") {
		t.Fatalf("expected refs/heads/trunk on the remote, got:\n%s", refs)
	}

	// A tag, then a deletion.
	e.run(src, "git", "tag", "v1")
	e.run(src, "git", "push", "gdrive", "v1")
	if refs := e.run(src, "git", "ls-remote", "gdrive"); !strings.Contains(refs, "refs/tags/v1") {
		t.Fatalf("expected the tag on the remote, got:\n%s", refs)
	}

	e.run(src, "git", "push", "gdrive", ":refs/heads/trunk")
	refs = e.run(src, "git", "ls-remote", "gdrive")
	if strings.Contains(refs, "refs/heads/trunk") {
		t.Fatalf("trunk should be deleted, got:\n%s", refs)
	}
	if !strings.Contains(refs, "refs/tags/v1") {
		t.Fatalf("deleting trunk should not disturb the tag, got:\n%s", refs)
	}
}

// TestHelperWorksWithEncryptedRepo checks the helper path through age, which
// is opt-in via --encrypt.
func TestHelperWorksWithEncryptedRepo(t *testing.T) {
	e := setup(t)
	src := e.newRepo("scripts")
	e.run(src, e.bin, "init", "--encrypt", "--name", "scripts")

	work := t.TempDir()
	e.run(work, "git", "clone", "gdrive://scripts", "clone")
	clone := filepath.Join(work, "clone")
	if got := e.readFile(clone, "deploy.sh"); got != "echo one\n" {
		t.Fatalf("encrypted clone mismatch: %q", got)
	}

	// Every stored link must be encrypted.
	entries, err := os.ReadDir(filepath.Join(e.store, "drive-git-remote", "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	var links int
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".bundle") {
			t.Fatalf("found an unencrypted link: %s", entry.Name())
		}
		if strings.HasSuffix(entry.Name(), ".age") {
			links++
		}
	}
	if links == 0 {
		t.Fatal("expected at least one encrypted link")
	}
}
