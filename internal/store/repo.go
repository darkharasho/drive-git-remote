package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/darkharasho/drive-git-remote/internal/crypto"
	"github.com/darkharasho/drive-git-remote/internal/gitx"
)

// Mirror ref namespace. These are real git refs in the local repo, so the
// last-synced remote state is durable and inspectable with plain git rather
// than living in a sidecar state file.
const (
	mirrorPrefix = "refs/drive/"
	headsPrefix  = "refs/heads/"
	tagsPrefix   = "refs/tags/"
)

func toMirror(ref string) string { return mirrorPrefix + strings.TrimPrefix(ref, "refs/") }

func fromMirror(ref string) string { return "refs/" + strings.TrimPrefix(ref, mirrorPrefix) }

// Repo binds a local git repository to its Drive folder.
type Repo struct {
	Backend  Backend
	FolderID string
	Meta     Meta
	Git      gitx.Repo
	// Ident is nil for unencrypted repos.
	Ident *crypto.Identity
}

// ReadMeta loads the immutable repo metadata from a Drive folder.
func ReadMeta(ctx context.Context, b Backend, folderID string) (Meta, error) {
	files, err := b.List(ctx, folderID)
	if err != nil {
		return Meta{}, err
	}
	for _, f := range files {
		if f.Name != MetaFile || f.Folder {
			continue
		}
		rc, err := b.Download(ctx, f.ID)
		if err != nil {
			return Meta{}, err
		}
		defer rc.Close()
		var m Meta
		if err := json.NewDecoder(rc).Decode(&m); err != nil {
			return Meta{}, fmt.Errorf("parsing %s: %w", MetaFile, err)
		}
		return m, nil
	}
	return Meta{}, fmt.Errorf("%s not found; folder is not a drive-git repo", MetaFile)
}

// WriteMeta writes repo metadata. Called once, at init.
func WriteMeta(ctx context.Context, b Backend, folderID string, m Meta) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	_, err = b.Upload(ctx, folderID, MetaFile, bytes.NewReader(body))
	return err
}

// Chain lists and validates the remote link chain.
func (r *Repo) Chain(ctx context.Context) ([]Link, error) {
	files, err := r.Backend.List(ctx, r.FolderID)
	if err != nil {
		return nil, err
	}
	return BuildChain(files)
}

// MirrorRefs returns the last-synced remote ref set, keyed by remote ref name.
func (r *Repo) MirrorRefs() (map[string]string, error) {
	raw, err := r.Git.Refs(mirrorPrefix)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string, len(raw))
	for name, sha := range raw {
		refs[fromMirror(name)] = sha
	}
	return refs, nil
}

// LocalRefs returns the local branches and tags that push publishes.
func (r *Repo) LocalRefs() (map[string]string, error) {
	return r.Git.Refs(headsPrefix, tagsPrefix)
}

func (r *Repo) setMirror(refs map[string]string) error {
	current, err := r.Git.Refs(mirrorPrefix)
	if err != nil {
		return err
	}
	want := make(map[string]string, len(refs))
	for name, sha := range refs {
		want[toMirror(name)] = sha
	}
	for name := range current {
		if _, keep := want[name]; !keep {
			if err := r.Git.DeleteRef(name); err != nil {
				return err
			}
		}
	}
	for name, sha := range want {
		if current[name] == sha {
			continue
		}
		if err := r.Git.SetRef(name, sha); err != nil {
			return err
		}
	}
	return nil
}

// fetchLink downloads a link (decrypting if needed) into a temp file.
func (r *Repo) fetchLink(ctx context.Context, l Link) (string, error) {
	if l.Encrypted && r.Ident == nil {
		return "", fmt.Errorf("%s is encrypted but no key is loaded", l.File.Name)
	}
	rc, err := r.Backend.Download(ctx, l.File.ID)
	if err != nil {
		return "", err
	}
	defer rc.Close()

	tmp, err := os.CreateTemp("", "drive-git-*"+l.Ext)
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	if l.Encrypted {
		err = r.Ident.Decrypt(tmp, rc)
	} else {
		_, err = io.Copy(tmp, rc)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// unbundle imports a bundle's objects and returns the ref set it declares,
// translated from the mirror namespace back to remote ref names.
//
// `git bundle unbundle` is the right plumbing here: it verifies prerequisites,
// writes objects, and updates no refs — leaving ref bookkeeping to us. It also
// avoids a nested `git fetch`, which matters because this same code path runs
// inside the remote helper, under a git process that is already running.
func (r *Repo) unbundle(path string) (map[string]string, error) {
	out, err := r.Git.Run("bundle", "unbundle", path)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if !strings.HasPrefix(name, mirrorPrefix) {
			return nil, fmt.Errorf("bundle declares unexpected ref %q", name)
		}
		refs[fromMirror(name)] = sha
	}
	return refs, nil
}

// applyLink imports a link's objects and sets the mirror refs to exactly the
// ref set the link declares.
func (r *Repo) applyLink(ctx context.Context, l Link) error {
	path, err := r.fetchLink(ctx, l)
	if err != nil {
		return err
	}
	defer os.Remove(path)

	var refs map[string]string
	if l.IsBundle() {
		if refs, err = r.unbundle(path); err != nil {
			return fmt.Errorf("applying %s: %w", l.File.Name, err)
		}
	} else {
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(body, &refs); err != nil {
			return fmt.Errorf("parsing %s: %w", l.File.Name, err)
		}
	}

	if got := TipHash(refs); got != l.Tip {
		return fmt.Errorf("%s is corrupt: contents hash to %s but the name claims %s",
			l.File.Name, got, l.Tip)
	}
	return r.setMirror(refs)
}

// Sync applies every remote link the local mirror has not yet seen. It touches
// only refs/drive/*, never the working tree or local branches.
func (r *Repo) Sync(ctx context.Context) (applied int, err error) {
	chain, err := r.Chain(ctx)
	if err != nil {
		return 0, err
	}
	mirror, err := r.MirrorRefs()
	if err != nil {
		return 0, err
	}

	start := 0
	if len(mirror) > 0 {
		local := TipHash(mirror)
		for i, l := range chain {
			if l.Tip == local {
				start = i + 1
				break
			}
		}
	}
	for _, l := range chain[start:] {
		if err := r.applyLink(ctx, l); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

// PushResult describes what a push did.
type PushResult struct {
	LinkName string
	Bytes    int64
	RefsOnly bool
	UpToDate bool
}

// ErrBehind means the remote moved since the last sync.
var ErrBehind = fmt.Errorf("remote has changes you do not have; run `drive-git pull` first")

// Push publishes local branches and tags as a new chain link.
func (r *Repo) Push(ctx context.Context) (*PushResult, error) {
	local, err := r.LocalRefs()
	if err != nil {
		return nil, err
	}
	if len(local) == 0 {
		return nil, fmt.Errorf("nothing to push: no branches or tags")
	}
	return r.PushRefs(ctx, local)
}

// PushRefs publishes an explicit remote ref set as a new chain link. The
// remote helper uses this to honour refspecs, including renaming pushes and
// deletions, which need not correspond to any local ref name.
func (r *Repo) PushRefs(ctx context.Context, target map[string]string) (*PushResult, error) {
	mirror, err := r.MirrorRefs()
	if err != nil {
		return nil, err
	}
	chain, err := r.Chain(ctx)
	if err != nil {
		return nil, err
	}
	remoteTip, mirrorTip := HeadTip(chain), RootTip
	if len(mirror) > 0 {
		mirrorTip = TipHash(mirror)
	}
	if remoteTip != mirrorTip {
		return nil, ErrBehind
	}
	newTip := TipHash(target)
	if newTip == mirrorTip {
		return &PushResult{UpToDate: true}, nil
	}

	lock, err := AcquireLock(ctx, r.Backend, r.FolderID)
	if err != nil {
		return nil, err
	}
	defer lock.Release(ctx, r.Backend)

	// Re-check under the lock: someone may have pushed between our listing
	// and our acquisition.
	if chain, err = r.Chain(ctx); err != nil {
		return nil, err
	}
	if HeadTip(chain) != mirrorTip {
		return nil, ErrBehind
	}

	// Bundles name their refs in the mirror namespace, so the mirror has to
	// hold the target set before the bundle is built. Roll it back if
	// anything from here on fails, so a failed push leaves no trace locally.
	notSHAs := gitx.SortedSHAs(mirror)
	if err := r.setMirror(target); err != nil {
		return nil, err
	}
	rollback := func() { _ = r.setMirror(mirror) }

	body, ext, err := r.buildLink(target, notSHAs)
	if err != nil {
		rollback()
		return nil, err
	}
	defer os.Remove(body)

	name := LinkName(NextSeq(chain), mirrorTip, newTip, ext, r.Ident != nil)
	size, err := r.uploadLink(ctx, name, body)
	if err != nil {
		rollback()
		return nil, err
	}

	// Confirm we are the only link at this position. A sibling means we lost
	// a race; withdraw our upload and leave the winner's chain intact.
	if _, err := r.Chain(ctx); err != nil {
		rollback()
		var fork *ForkError
		if ok := asForkError(err, &fork); ok {
			for _, f := range fork.Names {
				if f == name {
					_ = r.removeByName(ctx, name)
					return nil, fmt.Errorf("%w (another machine pushed at the same moment; your push was withdrawn)", ErrBehind)
				}
			}
		}
		return nil, err
	}
	return &PushResult{LinkName: name, Bytes: size, RefsOnly: ext == ExtRefs}, nil
}

// buildLink writes the payload for a new link and reports its extension.
//
// Every bundle names the complete current ref set, so a bundle's head list is
// authoritative for remote state while its objects stay incremental. The refs
// are named in the mirror namespace (refs/drive/...) rather than as
// refs/heads and refs/tags: git can only put a ref's real name into a bundle,
// and the remote ref names need not exist locally — a renaming push like
// `main:trunk` has no local refs/heads/trunk to bundle. The mirror namespace
// is ours to shape, so it can always carry the exact remote names.
//
// The caller must have set the mirror to target before calling this.
func (r *Repo) buildLink(target map[string]string, notSHAs []string) (path, ext string, err error) {
	tmp, err := os.CreateTemp("", "drive-git-push-*")
	if err != nil {
		return "", "", err
	}
	tmp.Close()
	path = tmp.Name()

	args := []string{"bundle", "create", path}
	for _, name := range gitx.SortedNames(target) {
		args = append(args, toMirror(name))
	}
	if len(notSHAs) > 0 {
		args = append(args, "--not")
		args = append(args, notSHAs...)
	}
	if _, err := r.Git.Run(args...); err != nil {
		if !strings.Contains(err.Error(), "empty bundle") {
			os.Remove(path)
			return "", "", err
		}
		// No new objects — a ref was deleted or moved onto an existing
		// commit. Record the ref set alone.
		body, mErr := json.MarshalIndent(target, "", "  ")
		if mErr != nil {
			os.Remove(path)
			return "", "", mErr
		}
		if wErr := os.WriteFile(path, body, 0o600); wErr != nil {
			os.Remove(path)
			return "", "", wErr
		}
		return path, ExtRefs, nil
	}
	return path, ExtBundle, nil
}

func (r *Repo) uploadLink(ctx context.Context, name, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var src io.Reader = f
	if r.Ident != nil {
		var buf bytes.Buffer
		if err := r.Ident.Encrypt(&buf, f); err != nil {
			return 0, err
		}
		src = &buf
	}
	up, err := r.Backend.Upload(ctx, r.FolderID, name, src)
	if err != nil {
		return 0, err
	}
	if up.Size > 0 {
		return up.Size, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, nil
	}
	return st.Size(), nil
}

func (r *Repo) removeByName(ctx context.Context, name string) error {
	files, err := r.Backend.List(ctx, r.FolderID)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.Name == name && !f.Folder {
			return r.Backend.Delete(ctx, f.ID)
		}
	}
	return nil
}

func asForkError(err error, target **ForkError) bool {
	for err != nil {
		if fe, ok := err.(*ForkError); ok {
			*target = fe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Compact replaces the chain with a single full bundle, archiving the old
// links. The local repo must be synced with the remote, since the new base is
// built from the mirror refs and every object behind them has to be present.
func (r *Repo) Compact(ctx context.Context) (string, error) {
	mirror, err := r.MirrorRefs()
	if err != nil {
		return "", err
	}
	chain, err := r.Chain(ctx)
	if err != nil {
		return "", err
	}
	if len(chain) == 0 {
		return "", fmt.Errorf("nothing to compact")
	}
	tip := TipHash(mirror)
	if len(mirror) == 0 || HeadTip(chain) != tip {
		return "", fmt.Errorf("compaction needs the local repo synced with the remote; run `drive-git pull` first")
	}

	lock, err := AcquireLock(ctx, r.Backend, r.FolderID)
	if err != nil {
		return "", err
	}
	defer lock.Release(ctx, r.Backend)

	tmp, err := os.CreateTemp("", "drive-git-base-*")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	args := []string{"bundle", "create", tmp.Name()}
	for _, name := range gitx.SortedNames(mirror) {
		args = append(args, toMirror(name))
	}
	if _, err := r.Git.Run(args...); err != nil {
		return "", err
	}

	name := LinkName(NextSeq(chain), RootTip, tip, ExtBundle, r.Ident != nil)
	if _, err := r.uploadLink(ctx, name, tmp.Name()); err != nil {
		return "", err
	}

	// Archive only after the new base is safely uploaded.
	archiveID, err := r.Backend.EnsureFolder(ctx, r.FolderID, ArchiveFolder)
	if err != nil {
		return "", err
	}
	for _, l := range chain {
		if err := r.Backend.Move(ctx, l.File.ID, archiveID); err != nil {
			return name, fmt.Errorf("archiving %s: %w", l.File.Name, err)
		}
	}
	return name, nil
}

// RemoveRepo deletes a repo folder and everything under it, reporting whether
// the removal is recoverable. Backends with a trash get the folder trashed;
// the rest are deleted depth-first, since a folder cannot be removed while it
// still has children.
func RemoveRepo(ctx context.Context, b Backend, folderID string) (recoverable bool, err error) {
	if t, ok := b.(Trasher); ok {
		return true, t.Trash(ctx, folderID)
	}
	return false, deleteTree(ctx, b, folderID)
}

func deleteTree(ctx context.Context, b Backend, folderID string) error {
	files, err := b.List(ctx, folderID)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.Folder {
			if err := deleteTree(ctx, b, f.ID); err != nil {
				return err
			}
			continue
		}
		if err := b.Delete(ctx, f.ID); err != nil {
			return fmt.Errorf("deleting %s: %w", f.Name, err)
		}
	}
	return b.Delete(ctx, folderID)
}

// CountLinks reports how many chain links a repo folder holds, including any
// archived by compaction, for confirmation prompts.
func CountLinks(ctx context.Context, b Backend, folderID string) (int, error) {
	files, err := b.List(ctx, folderID)
	if err != nil {
		return 0, err
	}
	var n int
	for _, f := range files {
		if f.Folder {
			if f.Name == ArchiveFolder {
				archived, err := b.List(ctx, f.ID)
				if err != nil {
					return 0, err
				}
				for _, a := range archived {
					if _, ok := ParseLink(a); ok {
						n++
					}
				}
			}
			continue
		}
		if _, ok := ParseLink(f); ok {
			n++
		}
	}
	return n, nil
}

// CloneInto initialises dir as a git repo and replays the chain into it.
func (r *Repo) CloneInto(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	r.Git = gitx.Repo{Dir: dir}
	if _, err := r.Git.Run("init", "--quiet"); err != nil {
		return err
	}
	if _, err := r.Sync(ctx); err != nil {
		return err
	}
	mirror, err := r.MirrorRefs()
	if err != nil {
		return err
	}
	for name, sha := range mirror {
		if strings.HasPrefix(name, headsPrefix) || strings.HasPrefix(name, tagsPrefix) {
			if err := r.Git.SetRef(name, sha); err != nil {
				return err
			}
		}
	}
	branch := r.pickBranch(mirror)
	if branch == "" {
		return nil // empty remote; leave the unborn branch as git created it
	}
	if _, err := r.Git.Run("symbolic-ref", "HEAD", headsPrefix+branch); err != nil {
		return err
	}
	_, err = r.Git.Run("checkout", "--quiet", branch)
	return err
}

func (r *Repo) pickBranch(refs map[string]string) string {
	for _, candidate := range []string{r.Meta.DefaultBranch, "main", "master"} {
		if candidate == "" {
			continue
		}
		if _, ok := refs[headsPrefix+candidate]; ok {
			return candidate
		}
	}
	for _, name := range gitx.SortedNames(refs) {
		if strings.HasPrefix(name, headsPrefix) {
			return strings.TrimPrefix(name, headsPrefix)
		}
	}
	return ""
}

// RepoDir is the local path of the bound repository.
func (r *Repo) RepoDir() string { return filepath.Clean(r.Git.Dir) }
