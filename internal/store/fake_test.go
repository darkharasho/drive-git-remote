package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// fakeBackend is an in-memory stand-in for Drive. It mirrors the two
// properties the chain depends on: creates never overwrite, and duplicate
// names in a folder are allowed.
type fakeBackend struct {
	mu     sync.Mutex
	nextID int
	files  map[string]*fakeFile

	// onUpload runs before every upload, letting a test inject a competing
	// write to simulate a race. Hooks that upload must guard against
	// re-entering themselves.
	onUpload func(parentID, name string)
}

type fakeFile struct {
	id       string
	name     string
	parent   string
	folder   bool
	contents []byte
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{files: map[string]*fakeFile{}}
}

func (b *fakeBackend) newID(prefix string) string {
	b.nextID++
	return fmt.Sprintf("%s%04d", prefix, b.nextID)
}

func (b *fakeBackend) FindFolder(_ context.Context, parentID, name string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, f := range b.files {
		if f.parent == parentID && f.name == name && f.folder {
			return f.id, nil
		}
	}
	return "", nil
}

func (b *fakeBackend) EnsureFolder(ctx context.Context, parentID, name string) (string, error) {
	if id, err := b.FindFolder(ctx, parentID, name); err != nil || id != "" {
		return id, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.newID("folder-")
	b.files[id] = &fakeFile{id: id, name: name, parent: parentID, folder: true}
	return id, nil
}

func (b *fakeBackend) List(_ context.Context, parentID string) ([]File, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []File
	for _, f := range b.files {
		if f.parent != parentID {
			continue
		}
		out = append(out, File{ID: f.id, Name: f.name, Size: int64(len(f.contents)), Folder: f.folder})
	}
	return out, nil
}

func (b *fakeBackend) Upload(_ context.Context, parentID, name string, r io.Reader) (File, error) {
	if b.onUpload != nil {
		b.onUpload(parentID, name)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return File{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.newID("file-")
	b.files[id] = &fakeFile{id: id, name: name, parent: parentID, contents: data}
	return File{ID: id, Name: name, Size: int64(len(data))}, nil
}

func (b *fakeBackend) Download(_ context.Context, id string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.files[id]
	if !ok {
		return nil, fmt.Errorf("no such file %s", id)
	}
	return io.NopCloser(bytes.NewReader(f.contents)), nil
}

func (b *fakeBackend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.files[id]; !ok {
		return fmt.Errorf("no such file %s", id)
	}
	delete(b.files, id)
	return nil
}

func (b *fakeBackend) Move(_ context.Context, id, newParentID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.files[id]
	if !ok {
		return fmt.Errorf("no such file %s", id)
	}
	f.parent = newParentID
	return nil
}

// names returns the sorted file names in a folder, for assertions.
func (b *fakeBackend) names(parentID string) []string {
	files, _ := b.List(context.Background(), parentID)
	var out []string
	for _, f := range files {
		out = append(out, f.Name)
	}
	return out
}
