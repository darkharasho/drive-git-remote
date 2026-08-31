// Package local implements the store backend against a plain directory.
//
// It exists so the tool can be exercised without OAuth — the end-to-end tests
// drive real git against it — and as an escape hatch for pointing at a folder
// that something else syncs (a Drive desktop client, a USB stick). Set
// DRIVE_GIT_LOCAL_ROOT to use it instead of the Drive API.
//
// One deliberate difference from Drive: a filesystem cannot hold two files
// with the same name in a directory, so Upload overwrites where Drive would
// create a sibling. The append-only chain never overwrites a link in normal
// operation, but this does mean simultaneous pushes are not detectable here
// the way they are against Drive.
package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/darkharasho/drive-git-remote/internal/store"
)

// EnvRoot names the environment variable that selects this backend.
const EnvRoot = "DRIVE_GIT_LOCAL_ROOT"

// Backend stores repos under a directory tree. IDs are paths relative to root.
type Backend struct {
	root string
}

var _ store.Backend = (*Backend)(nil)

// New opens (and creates) a directory-backed store.
func New(root string) (*Backend, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &Backend{root: abs}, nil
}

// path resolves an ID to an absolute path, refusing to escape the root.
func (b *Backend) path(id string) (string, error) {
	clean := filepath.Clean(filepath.Join(b.root, id))
	if clean != b.root && !strings.HasPrefix(clean, b.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the store root", id)
	}
	return clean, nil
}

func (b *Backend) id(abs string) string {
	rel, err := filepath.Rel(b.root, abs)
	if err != nil {
		return abs
	}
	return rel
}

// FindFolder returns the ID of a child folder, or "" if absent.
func (b *Backend) FindFolder(_ context.Context, parentID, name string) (string, error) {
	p, err := b.path(filepath.Join(parentID, name))
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if !st.IsDir() {
		return "", nil
	}
	return b.id(p), nil
}

// EnsureFolder finds or creates a child folder.
func (b *Backend) EnsureFolder(_ context.Context, parentID, name string) (string, error) {
	p, err := b.path(filepath.Join(parentID, name))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return b.id(p), nil
}

// List returns the immediate children of a folder.
func (b *Backend) List(_ context.Context, parentID string) ([]store.File, error) {
	p, err := b.path(parentID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []store.File
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, store.File{
			ID:     b.id(filepath.Join(p, e.Name())),
			Name:   e.Name(),
			Size:   info.Size(),
			Folder: e.IsDir(),
		})
	}
	return out, nil
}

// Upload writes a file, replacing any file of the same name.
func (b *Backend) Upload(_ context.Context, parentID, name string, r io.Reader) (store.File, error) {
	p, err := b.path(filepath.Join(parentID, name))
	if err != nil {
		return store.File{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return store.File{}, err
	}
	// Write to a temp file and rename, so a killed process cannot leave a
	// truncated link behind.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return store.File{}, err
	}
	n, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return store.File{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return store.File{}, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return store.File{}, err
	}
	return store.File{ID: b.id(p), Name: name, Size: n}, nil
}

// Download opens a file's contents.
func (b *Backend) Download(_ context.Context, id string) (io.ReadCloser, error) {
	p, err := b.path(id)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// Delete removes a file.
func (b *Backend) Delete(_ context.Context, id string) error {
	p, err := b.path(id)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// Move reparents a file.
func (b *Backend) Move(_ context.Context, id, newParentID string) error {
	src, err := b.path(id)
	if err != nil {
		return err
	}
	dst, err := b.path(filepath.Join(newParentID, filepath.Base(src)))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.Rename(src, dst)
}
