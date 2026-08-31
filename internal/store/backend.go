package store

import (
	"context"
	"io"
)

// File is a Drive object (folder or regular file).
type File struct {
	ID     string
	Name   string
	Size   int64
	Folder bool
}

// Backend is the storage surface the bundle chain needs. It is deliberately
// small so the whole store can be exercised against an in-memory fake.
//
// The chain only ever creates and reads files; the sole destructive operations
// (Delete, Move) are used for lock release, losing-side race cleanup, and
// compaction — never on the live chain.
type Backend interface {
	// EnsureFolder returns the ID of the named child folder under parentID,
	// creating it if absent. A parentID of "" means the Drive root.
	EnsureFolder(ctx context.Context, parentID, name string) (string, error)
	// FindFolder returns the ID of a child folder, or "" if it does not exist.
	FindFolder(ctx context.Context, parentID, name string) (string, error)
	// List returns the immediate children of a folder.
	List(ctx context.Context, parentID string) ([]File, error)
	// Upload creates a new file. It never overwrites: Drive permits duplicate
	// names in a folder, which is what makes racing pushes detectable rather
	// than destructive.
	Upload(ctx context.Context, parentID, name string, r io.Reader) (File, error)
	// Download opens a file's contents.
	Download(ctx context.Context, id string) (io.ReadCloser, error)
	// Delete removes a file.
	Delete(ctx context.Context, id string) error
	// Move reparents a file.
	Move(ctx context.Context, id, newParentID string) error
}
