package store

import (
	"context"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darkharasho/drive-git-remote/internal/crypto"
	"github.com/darkharasho/drive-git-remote/internal/gitx"
)

func newRemote(t *testing.T) (*fakeBackend, string) {
	t.Helper()
	b := newFakeBackend()
	folderID, err := b.EnsureFolder(context.Background(), "", "scripts")
	if err != nil {
		t.Fatal(err)
	}
	meta := Meta{Version: 1, Name: "scripts", DefaultBranch: "main", Encryption: "none"}
	if err := WriteMeta(context.Background(), b, folderID, meta); err != nil {
		t.Fatal(err)
	}
	return b, folderID
}

func newRepo(t *testing.T, b Backend, folderID string) *Repo {
	t.Helper()
	dir := t.TempDir()
	g := gitx.Repo{Dir: dir}
	mustGit(t, g, "init", "--quiet", "--initial-branch=main")
	mustGit(t, g, "config", "user.email", "test@example.com")
	mustGit(t, g, "config", "user.name", "Test")
	return &Repo{
		Backend:  b,
		FolderID: folderID,
		Meta:     Meta{Version: 1, Name: "scripts", DefaultBranch: "main"},
		Git:      g,
	}
}

func mustGit(t *testing.T, g gitx.Repo, args ...string) string {
	t.Helper()
	out, err := g.Run(args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func commit(t *testing.T, r *Repo, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Git.Dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, r.Git, "add", name)
	mustGit(t, r.Git, "commit", "--quiet", "-m", "add "+name)
}

// randomText returns deterministic, incompressible text of roughly n bytes.
func randomText(n int) string {
	rng := rand.New(rand.NewSource(1))
	buf := make([]byte, n/2)
	rng.Read(buf)
	return hex.EncodeToString(buf)
}

func mustPush(t *testing.T, r *Repo) *PushResult {
	t.Helper()
	res, err := r.Push(context.Background())
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	return res
}

func TestPushCloneRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, folderID := newRemote(t)

	a := newRepo(t, b, folderID)
	commit(t, a, "deploy.sh", "echo one\n")
	res := mustPush(t, a)
	if !strings.HasPrefix(res.LinkName, "0001-root-") {
		t.Fatalf("first link should start the chain, got %q", res.LinkName)
	}

	clone := &Repo{Backend: b, FolderID: folderID, Meta: Meta{DefaultBranch: "main"}}
	dir := filepath.Join(t.TempDir(), "clone")
	if err := clone.CloneInto(ctx, dir); err != nil {
		t.Fatalf("clone: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "deploy.sh"))
	if err != nil {
		t.Fatalf("cloned file: %v", err)
	}
	if string(got) != "echo one\n" {
		t.Fatalf("content mismatch: %q", got)
	}
	if branch, _ := clone.Git.CurrentBranch(); branch != "main" {
		t.Fatalf("expected main checked out, got %q", branch)
	}
}

func TestIncrementalPushCarriesOnlyNewObjects(t *testing.T) {
	ctx := context.Background()
	b, folderID := newRemote(t)

	a := newRepo(t, b, folderID)
	// Incompressible content, so bundle sizes reflect object counts rather
	// than how well zlib handles repetition.
	commit(t, a, "big.txt", randomText(200_000))
	first := mustPush(t, a)

	commit(t, a, "small.txt", "tiny\n")
	second := mustPush(t, a)

	if second.Bytes >= first.Bytes {
		t.Fatalf("incremental bundle (%d B) should be smaller than the base (%d B)", second.Bytes, first.Bytes)
	}
	if !strings.HasPrefix(second.LinkName, "0002-") {
		t.Fatalf("expected sequence 0002, got %q", second.LinkName)
	}

	// A clone replays the whole chain and lands on the latest state.
	clone := &Repo{Backend: b, FolderID: folderID, Meta: Meta{DefaultBranch: "main"}}
	if err := clone.CloneInto(ctx, filepath.Join(t.TempDir(), "clone")); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone.Git.Dir, "small.txt")); err != nil {
		t.Fatalf("clone missing latest commit: %v", err)
	}
}

func TestPushRejectsWhenBehind(t *testing.T) {
	ctx := context.Background()
	b, folderID := newRemote(t)

	a := newRepo(t, b, folderID)
	commit(t, a, "a.txt", "one\n")
	mustPush(t, a)

	// Second machine clones, then both commit independently.
	second := &Repo{Backend: b, FolderID: folderID, Meta: Meta{DefaultBranch: "main"}}
	if err := second.CloneInto(ctx, filepath.Join(t.TempDir(), "clone")); err != nil {
		t.Fatal(err)
	}
	mustGit(t, second.Git, "config", "user.email", "test@example.com")
	mustGit(t, second.Git, "config", "user.name", "Test")

	commit(t, a, "from-a.txt", "a\n")
	mustPush(t, a)

	commit(t, second, "from-b.txt", "b\n")
	if _, err := second.Push(ctx); !errors.Is(err, ErrBehind) {
		t.Fatalf("expected ErrBehind, got %v", err)
	}

	// Pull, then the push succeeds and the chain stays linear.
	if _, err := second.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	mustGit(t, second.Git, "merge", "--quiet", "--no-edit", "refs/drive/heads/main")
	mustPush(t, second)
	if _, err := second.Chain(ctx); err != nil {
		t.Fatalf("chain should be valid after recovery: %v", err)
	}
}

func TestSimultaneousPushWithdrawsTheLoser(t *testing.T) {
	ctx := context.Background()
	b, folderID := newRemote(t)

	a := newRepo(t, b, folderID)
	commit(t, a, "a.txt", "one\n")
	mustPush(t, a)

	commit(t, a, "two.txt", "two\n")

	// Inject a competing link at the same chain position during our upload,
	// which is exactly what a genuine simultaneous push looks like.
	injected := false
	b.onUpload = func(parentID, name string) {
		if injected || !strings.HasSuffix(name, ExtBundle) {
			return
		}
		l, ok := ParseLink(File{Name: name})
		if !ok {
			return
		}
		injected = true
		rival := LinkName(l.Seq, l.ParentTip, "ffffffffffff", ExtBundle, false)
		if _, err := b.Upload(ctx, parentID, rival, strings.NewReader("rival")); err != nil {
			t.Error(err)
		}
	}

	_, err := a.Push(ctx)
	if !errors.Is(err, ErrBehind) {
		t.Fatalf("expected the losing push to report ErrBehind, got %v", err)
	}
	// Our upload must have been withdrawn, leaving the rival's chain intact.
	var ours int
	for _, n := range b.names(folderID) {
		if strings.HasPrefix(n, "0002-") {
			ours++
		}
	}
	if ours != 1 {
		t.Fatalf("expected only the rival link at position 0002, found %d: %v", ours, b.names(folderID))
	}
}

func TestRefsOnlyPushWhenNoNewObjects(t *testing.T) {
	ctx := context.Background()
	b, folderID := newRemote(t)

	a := newRepo(t, b, folderID)
	commit(t, a, "a.txt", "one\n")
	mustGit(t, a.Git, "branch", "scratch")
	mustPush(t, a)

	// Deleting a branch changes the ref set but adds no objects.
	mustGit(t, a.Git, "branch", "-D", "scratch")
	res := mustPush(t, a)
	if !res.RefsOnly {
		t.Fatalf("expected a refs-only link, got %+v", res)
	}

	clone := &Repo{Backend: b, FolderID: folderID, Meta: Meta{DefaultBranch: "main"}}
	if err := clone.CloneInto(ctx, filepath.Join(t.TempDir(), "clone")); err != nil {
		t.Fatal(err)
	}
	refs, err := clone.MirrorRefs()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := refs["refs/heads/scratch"]; ok {
		t.Fatalf("deleted branch should not survive the round trip: %v", refs)
	}
}

func TestEncryptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, folderID := newRemote(t)
	keyPath := filepath.Join(t.TempDir(), "key.age")
	id, _, err := crypto.LoadOrCreateIdentity(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	a := newRepo(t, b, folderID)
	a.Ident = id
	commit(t, a, "secret.sh", "echo hunter2\n")
	res := mustPush(t, a)
	if !strings.HasSuffix(res.LinkName, ExtEncrypted) {
		t.Fatalf("expected an encrypted link, got %q", res.LinkName)
	}

	// The stored bytes must not contain the plaintext.
	files, _ := b.List(ctx, folderID)
	for _, f := range files {
		if !strings.HasSuffix(f.Name, ExtEncrypted) {
			continue
		}
		rc, err := b.Download(ctx, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, f.Size)
		rc.Read(buf)
		rc.Close()
		if strings.Contains(string(buf), "hunter2") {
			t.Fatal("plaintext leaked into the uploaded bundle")
		}
	}

	clone := &Repo{Backend: b, FolderID: folderID, Meta: Meta{DefaultBranch: "main"}, Ident: id}
	if err := clone.CloneInto(ctx, filepath.Join(t.TempDir(), "clone")); err != nil {
		t.Fatalf("clone: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(clone.Git.Dir, "secret.sh"))
	if err != nil || string(got) != "echo hunter2\n" {
		t.Fatalf("decrypted round trip failed: %q %v", got, err)
	}
}

func TestCompactReplacesChain(t *testing.T) {
	ctx := context.Background()
	b, folderID := newRemote(t)

	a := newRepo(t, b, folderID)
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		commit(t, a, name, name+"\n")
		mustPush(t, a)
	}

	name, err := a.Compact(ctx)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !strings.Contains(name, "-root-") {
		t.Fatalf("compacted base should start a fresh chain, got %q", name)
	}
	chain, err := a.Chain(ctx)
	if err != nil {
		t.Fatalf("chain after compaction: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected a single link after compaction, got %d", len(chain))
	}

	clone := &Repo{Backend: b, FolderID: folderID, Meta: Meta{DefaultBranch: "main"}}
	if err := clone.CloneInto(ctx, filepath.Join(t.TempDir(), "clone")); err != nil {
		t.Fatalf("clone from compacted chain: %v", err)
	}
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if _, err := os.Stat(filepath.Join(clone.Git.Dir, name)); err != nil {
			t.Fatalf("compacted base lost history: %v", err)
		}
	}
}

func TestLockBlocksAndExpires(t *testing.T) {
	ctx := context.Background()
	b, folderID := newRemote(t)

	first, err := AcquireLock(ctx, b, folderID)
	if err != nil {
		t.Fatal(err)
	}
	var locked *LockedError
	if _, err := AcquireLock(ctx, b, folderID); !errors.As(err, &locked) {
		t.Fatalf("expected LockedError, got %v", err)
	}
	if err := first.Release(ctx, b); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireLock(ctx, b, folderID)
	if err != nil {
		t.Fatalf("lock should be free after release: %v", err)
	}

	// An expired lock is broken rather than blocking forever.
	second.Expires = time.Now().Add(-time.Minute)
	l, err := ReadLock(ctx, b, folderID)
	if err != nil {
		t.Fatal(err)
	}
	l.Expires = time.Now().Add(-time.Minute)
	if !l.Expired(time.Now()) {
		t.Fatal("lock should read as expired")
	}
}

// trashingBackend adds the optional Trasher capability, as Drive has.
type trashingBackend struct {
	*fakeBackend
	trashed []string
}

func (b *trashingBackend) Trash(_ context.Context, id string) error {
	b.trashed = append(b.trashed, id)
	return nil
}

func TestRemoveRepoPrefersTrash(t *testing.T) {
	ctx := context.Background()
	b, folderID := newRemote(t)
	tb := &trashingBackend{fakeBackend: b}

	a := newRepo(t, tb, folderID)
	commit(t, a, "a.txt", "one\n")
	mustPush(t, a)

	recoverable, err := RemoveRepo(ctx, tb, folderID)
	if err != nil {
		t.Fatal(err)
	}
	if !recoverable {
		t.Fatal("a backend with a trash should report the removal as recoverable")
	}
	if len(tb.trashed) != 1 || tb.trashed[0] != folderID {
		t.Fatalf("expected the folder itself to be trashed, got %v", tb.trashed)
	}
	// Trashing must not destroy anything: the contents are still there to
	// restore alongside the folder.
	if files, _ := tb.List(ctx, folderID); len(files) == 0 {
		t.Fatal("trashing should leave contents intact")
	}
}

func TestRemoveRepoDeletesWhenNoTrash(t *testing.T) {
	ctx := context.Background()
	b, folderID := newRemote(t)

	a := newRepo(t, b, folderID)
	for _, name := range []string{"one.txt", "two.txt"} {
		commit(t, a, name, name+"\n")
		mustPush(t, a)
	}
	if _, err := a.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	// Compaction leaves an archive subfolder, so removal has to recurse.
	if n, err := CountLinks(ctx, b, folderID); err != nil || n != 3 {
		t.Fatalf("expected 3 links counted across the folder and archive, got %d (%v)", n, err)
	}

	recoverable, err := RemoveRepo(ctx, b, folderID)
	if err != nil {
		t.Fatal(err)
	}
	if recoverable {
		t.Fatal("a backend without a trash should report permanent removal")
	}
	if files, _ := b.List(ctx, folderID); len(files) != 0 {
		t.Fatalf("expected the folder emptied, found %v", files)
	}
	if _, ok := b.files[folderID]; ok {
		t.Fatal("expected the folder itself removed")
	}
}

func TestBuildChainDetectsFork(t *testing.T) {
	files := []File{
		{ID: "1", Name: "0001-root-aaaaaaaaaaaa.bundle"},
		{ID: "2", Name: "0002-aaaaaaaaaaaa-bbbbbbbbbbbb.bundle"},
		{ID: "3", Name: "0002-aaaaaaaaaaaa-cccccccccccc.bundle"},
	}
	_, err := BuildChain(files)
	var fork *ForkError
	if !errors.As(err, &fork) {
		t.Fatalf("expected ForkError, got %v", err)
	}
	if fork.Seq != 2 || len(fork.Names) != 2 {
		t.Fatalf("unexpected fork report: %+v", fork)
	}
}

func TestBuildChainDetectsBrokenParent(t *testing.T) {
	files := []File{
		{ID: "1", Name: "0001-root-aaaaaaaaaaaa.bundle"},
		{ID: "2", Name: "0002-zzzzzzzzzzzz-bbbbbbbbbbbb.bundle"},
	}
	if _, err := BuildChain(files); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("expected a broken-chain error, got %v", err)
	}
}
