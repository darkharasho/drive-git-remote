// Package session holds the wiring shared by the CLI and the remote helper:
// authenticating, resolving the Drive root, and binding a repo to its folder.
package session

import (
	"context"
	"fmt"
	"os"

	"github.com/darkharasho/drive-git-remote/internal/auth"
	"github.com/darkharasho/drive-git-remote/internal/config"
	"github.com/darkharasho/drive-git-remote/internal/crypto"
	"github.com/darkharasho/drive-git-remote/internal/drive"
	"github.com/darkharasho/drive-git-remote/internal/gitx"
	"github.com/darkharasho/drive-git-remote/internal/local"
	"github.com/darkharasho/drive-git-remote/internal/store"
)

// Backend returns the configured storage backend: Drive, or a local directory
// when DRIVE_GIT_LOCAL_ROOT is set.
func Backend(ctx context.Context) (store.Backend, error) {
	if root := os.Getenv(local.EnvRoot); root != "" {
		return local.New(root)
	}
	hc, err := auth.Client(ctx)
	if err != nil {
		return nil, err
	}
	return drive.New(ctx, hc)
}

// RootFolder returns the ID of the single drive-git-remote/ folder.
func RootFolder(ctx context.Context, b store.Backend) (string, error) {
	return b.EnsureFolder(ctx, "", config.RootFolderName)
}

// FindRepoFolder resolves a repo name to its Drive folder ID.
func FindRepoFolder(ctx context.Context, b store.Backend, name string) (string, error) {
	rootID, err := RootFolder(ctx, b)
	if err != nil {
		return "", err
	}
	folderID, err := b.FindFolder(ctx, rootID, name)
	if err != nil {
		return "", err
	}
	if folderID == "" {
		return "", fmt.Errorf("no repo named %q in Drive (try `drive-git list`)", name)
	}
	return folderID, nil
}

// Open binds a Drive folder to a local git repo, loading the encryption key.
func Open(ctx context.Context, b store.Backend, folderID string, g gitx.Repo) (*store.Repo, error) {
	meta, err := store.ReadMeta(ctx, b, folderID)
	if err != nil {
		return nil, err
	}
	r := &store.Repo{Backend: b, FolderID: folderID, Meta: meta, Git: g}
	if err := AttachKey(r); err != nil {
		return nil, err
	}
	return r, nil
}

// OpenByName resolves a repo name and binds it to a local git repo.
func OpenByName(ctx context.Context, name string, g gitx.Repo) (*store.Repo, error) {
	b, err := Backend(ctx)
	if err != nil {
		return nil, err
	}
	folderID, err := FindRepoFolder(ctx, b, name)
	if err != nil {
		return nil, err
	}
	return Open(ctx, b, folderID, g)
}

// AttachKey loads the age identity for an encrypted repo and checks it is the
// right one, so a wrong key fails clearly instead of as a decryption error.
func AttachKey(r *store.Repo) error {
	if r.Meta.Encryption != "age" {
		return nil
	}
	p, err := config.KeyPath()
	if err != nil {
		return err
	}
	id, err := crypto.LoadIdentity(p)
	if err != nil {
		return err
	}
	if r.Meta.Recipient != "" && id.Recipient() != r.Meta.Recipient {
		return fmt.Errorf("key at %s does not match this repo (expected %s); copy the key from the machine that ran `drive-git init`",
			p, r.Meta.Recipient)
	}
	r.Ident = id
	return nil
}
