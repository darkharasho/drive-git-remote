// Package cli wires the command surface.
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/darkharasho/drive-git-remote/internal/auth"
	"github.com/darkharasho/drive-git-remote/internal/gitx"
	"github.com/darkharasho/drive-git-remote/internal/session"
	"github.com/darkharasho/drive-git-remote/internal/store"
	"github.com/spf13/cobra"
)

// Version is set at build time.
var Version = "dev"

// NewRoot builds the command tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "drive-git",
		Short:         "Use a private Google Drive folder as a git remote",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newSetupCmd(), newLoginCmd(), newLogoutCmd(), newWhoamiCmd(),
		newInitCmd(), newCloneCmd(), newPushCmd(), newPullCmd(),
		newListCmd(), newStatusCmd(), newUnlockCmd(), newGCCmd(),
		newInstallHelperCmd(), newRemoteHelperCmd(),
	)
	return root
}

// Execute runs the CLI.
func Execute() int {
	if err := NewRoot().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if auth.IsLoginExpired(err) {
			fmt.Fprintln(os.Stderr, "\n"+auth.ExpiryHint)
		}
		return 1
	}
	return 0
}

// backend returns an authenticated Drive backend.
func backend(ctx context.Context) (store.Backend, error) { return session.Backend(ctx) }

// rootFolder returns the ID of the single drive-git-remote/ folder.
func rootFolder(ctx context.Context, b store.Backend) (string, error) {
	return session.RootFolder(ctx, b)
}

// Local git config keys binding a working repo to its Drive folder.
const (
	cfgFolderID = "drive.folder-id"
	cfgName     = "drive.name"
)

// openRepo binds the git repo containing the working directory to its remote.
func openRepo(ctx context.Context) (*store.Repo, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	top, err := gitx.Toplevel(wd)
	if err != nil {
		return nil, fmt.Errorf("not inside a git repository")
	}
	g := gitx.Repo{Dir: top}
	folderID := g.Config(cfgFolderID)
	if folderID == "" {
		return nil, fmt.Errorf("this repo is not linked to Drive; run `drive-git init` here, or `drive-git clone <name>`")
	}

	b, err := backend(ctx)
	if err != nil {
		return nil, err
	}
	return session.Open(ctx, b, folderID, g)
}

func attachKey(r *store.Repo) error { return session.AttachKey(r) }
