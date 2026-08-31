package drive_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/darkharasho/drive-git-remote/internal/session"
	"github.com/darkharasho/drive-git-remote/internal/store"
)

// These tests talk to the real Drive API using the logged-in account. They are
// skipped unless DRIVE_GIT_LIVE=1, and clean up after themselves.
//
//	DRIVE_GIT_LIVE=1 go test ./internal/drive/ -v
func liveBackend(t *testing.T) (store.Backend, string) {
	t.Helper()
	if os.Getenv("DRIVE_GIT_LIVE") != "1" {
		t.Skip("set DRIVE_GIT_LIVE=1 to run tests against real Drive")
	}
	if os.Getenv("DRIVE_GIT_LOCAL_ROOT") != "" {
		t.Fatal("DRIVE_GIT_LOCAL_ROOT is set; these tests must run against Drive")
	}
	ctx := context.Background()
	b, err := session.Backend(ctx)
	if err != nil {
		t.Fatalf("connecting to Drive: %v", err)
	}
	rootID, err := session.RootFolder(ctx, b)
	if err != nil {
		t.Fatalf("resolving root folder: %v", err)
	}
	name := fmt.Sprintf("dgr-livetest-%d", time.Now().UnixNano())
	folderID, err := b.EnsureFolder(ctx, rootID, name)
	if err != nil {
		t.Fatalf("creating scratch folder: %v", err)
	}
	t.Cleanup(func() {
		files, err := b.List(ctx, folderID)
		if err == nil {
			for _, f := range files {
				_ = b.Delete(ctx, f.ID)
			}
		}
		if err := b.Delete(ctx, folderID); err != nil {
			t.Logf("could not remove scratch folder %s: %v", name, err)
		}
	})
	return b, folderID
}

// TestDriveAllowsDuplicateNames verifies the property the append-only chain
// depends on for safety. Drive is name-addressed only loosely: two files may
// share a name in one folder, each with its own ID. That is what turns a
// simultaneous push into a detectable sibling rather than a silent overwrite.
// If Drive ever changed this, races would corrupt the chain instead of being
// caught, so it is worth asserting directly.
func TestDriveAllowsDuplicateNames(t *testing.T) {
	ctx := context.Background()
	b, folderID := liveBackend(t)

	const name = "0001-root-aaaaaaaaaaaa.bundle"
	first, err := b.Upload(ctx, folderID, name, strings.NewReader("first"))
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	second, err := b.Upload(ctx, folderID, name, strings.NewReader("second-longer"))
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("Drive reused the file ID; uploads are overwriting, so racing pushes would be lost")
	}

	files, err := b.List(ctx, folderID)
	if err != nil {
		t.Fatal(err)
	}
	var matches int
	for _, f := range files {
		if f.Name == name {
			matches++
		}
	}
	if matches != 2 {
		t.Fatalf("expected 2 files named %s, found %d", name, matches)
	}

	// Both survive independently.
	for _, f := range []store.File{first, second} {
		rc, err := b.Download(ctx, f.ID)
		if err != nil {
			t.Fatalf("downloading %s: %v", f.ID, err)
		}
		rc.Close()
	}
}

// TestForkDetectionAgainstDrive drives the real listing through the chain
// validator, which is how a lost push race actually surfaces.
func TestForkDetectionAgainstDrive(t *testing.T) {
	ctx := context.Background()
	b, folderID := liveBackend(t)

	for _, name := range []string{
		"0001-root-aaaaaaaaaaaa.bundle",
		"0002-aaaaaaaaaaaa-bbbbbbbbbbbb.bundle",
		"0002-aaaaaaaaaaaa-cccccccccccc.bundle",
	} {
		if _, err := b.Upload(ctx, folderID, name, strings.NewReader("x")); err != nil {
			t.Fatalf("uploading %s: %v", name, err)
		}
	}
	files, err := b.List(ctx, folderID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.BuildChain(files)
	var fork *store.ForkError
	if !asFork(err, &fork) {
		t.Fatalf("expected a ForkError from the real listing, got %v", err)
	}
	if fork.Seq != 2 {
		t.Fatalf("expected the fork at position 2, got %d", fork.Seq)
	}
}

func asFork(err error, target **store.ForkError) bool {
	fe, ok := err.(*store.ForkError)
	if ok {
		*target = fe
	}
	return ok
}

// TestLockRoundTripAgainstDrive exercises the advisory lock on real Drive.
func TestLockRoundTripAgainstDrive(t *testing.T) {
	ctx := context.Background()
	b, folderID := liveBackend(t)

	lock, err := store.AcquireLock(ctx, b, folderID)
	if err != nil {
		t.Fatalf("acquiring: %v", err)
	}
	var held *store.LockedError
	if _, err := store.AcquireLock(ctx, b, folderID); err == nil {
		t.Fatal("expected the second acquisition to be blocked")
	} else if !asLocked(err, &held) {
		t.Fatalf("expected LockedError, got %v", err)
	}
	if err := lock.Release(ctx, b); err != nil {
		t.Fatalf("releasing: %v", err)
	}
	again, err := store.AcquireLock(ctx, b, folderID)
	if err != nil {
		t.Fatalf("re-acquiring after release: %v", err)
	}
	if err := again.Release(ctx, b); err != nil {
		t.Fatal(err)
	}
}

func asLocked(err error, target **store.LockedError) bool {
	le, ok := err.(*store.LockedError)
	if ok {
		*target = le
	}
	return ok
}
