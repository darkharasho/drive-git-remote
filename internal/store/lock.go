package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// LockTTL is how long a push lock stays valid before other machines may break
// it. Pushes are seconds-long; this only needs to outlast a slow upload.
const LockTTL = 10 * time.Minute

// Lock is advisory. Drive v3 has no compare-and-swap (ETag preconditions were
// dropped), so this cannot be a correctness mechanism — it exists to stop two
// machines wasting an upload. Correctness comes from the append-only chain:
// a lost race produces a detectable sibling link, never a destructive write.
type Lock struct {
	Holder   string    `json:"holder"`
	Host     string    `json:"host"`
	User     string    `json:"user"`
	Acquired time.Time `json:"acquired"`
	Expires  time.Time `json:"expires"`

	fileID string
}

// Expired reports whether the lock may be broken.
func (l *Lock) Expired(now time.Time) bool { return now.After(l.Expires) }

// Describe renders the holder for an error message.
func (l *Lock) Describe() string {
	return fmt.Sprintf("%s@%s since %s", l.User, l.Host, l.Acquired.Local().Format(time.RFC1123))
}

// LockedError reports that another machine holds the push lock.
type LockedError struct{ Lock *Lock }

func (e *LockedError) Error() string {
	return fmt.Sprintf("push lock held by %s; it expires at %s (use `drive-git unlock` to break it)",
		e.Lock.Describe(), e.Lock.Expires.Local().Format(time.Kitchen))
}

// ReadLock returns the current lock, or nil when the folder is unlocked.
func ReadLock(ctx context.Context, b Backend, folderID string) (*Lock, error) {
	files, err := b.List(ctx, folderID)
	if err != nil {
		return nil, err
	}
	var found *File
	for i := range files {
		if files[i].Name == LockFile && !files[i].Folder {
			found = &files[i]
			break
		}
	}
	if found == nil {
		return nil, nil
	}
	rc, err := b.Download(ctx, found.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		// An unparseable lock is treated as stale rather than fatal; it is
		// still guarded by the append-only chain.
		l = Lock{Host: "unknown", User: "unknown"}
	}
	l.fileID = found.ID
	return &l, nil
}

// AcquireLock takes the push lock, breaking an expired one. It re-lists after
// creating to catch the case where two machines created a lock concurrently;
// the higher holder ID yields.
func AcquireLock(ctx context.Context, b Backend, folderID string) (*Lock, error) {
	now := time.Now()
	existing, err := ReadLock(ctx, b, folderID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !existing.Expired(now) {
			return nil, &LockedError{Lock: existing}
		}
		if err := b.Delete(ctx, existing.fileID); err != nil {
			return nil, fmt.Errorf("breaking expired lock: %w", err)
		}
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}
	l := &Lock{
		Holder:   hex.EncodeToString(buf),
		Host:     host,
		User:     user,
		Acquired: now,
		Expires:  now.Add(LockTTL),
	}
	body, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	f, err := b.Upload(ctx, folderID, LockFile, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	l.fileID = f.ID

	// Drive allows duplicate names, so a concurrent acquirer leaves a second
	// .lock behind. Deterministically pick a winner by lowest holder ID.
	files, err := b.List(ctx, folderID)
	if err != nil {
		return nil, err
	}
	for _, other := range files {
		if other.Name != LockFile || other.ID == l.fileID || other.Folder {
			continue
		}
		if other.ID < l.fileID {
			_ = b.Delete(ctx, l.fileID)
			return nil, fmt.Errorf("lost a concurrent lock acquisition; try again")
		}
	}
	return l, nil
}

// Release drops the lock. It is safe to call on an already-broken lock.
func (l *Lock) Release(ctx context.Context, b Backend) error {
	if l == nil || l.fileID == "" {
		return nil
	}
	return b.Delete(ctx, l.fileID)
}

// BreakLock force-removes any lock present.
func BreakLock(ctx context.Context, b Backend, folderID string) (bool, error) {
	l, err := ReadLock(ctx, b, folderID)
	if err != nil || l == nil {
		return false, err
	}
	return true, b.Delete(ctx, l.fileID)
}
