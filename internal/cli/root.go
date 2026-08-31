// Package cli wires the command surface.
package cli

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/darkharasho/drive-git-remote/internal/auth"
	"github.com/darkharasho/drive-git-remote/internal/gitx"
	"github.com/darkharasho/drive-git-remote/internal/session"
	"github.com/darkharasho/drive-git-remote/internal/store"
	"github.com/darkharasho/drive-git-remote/internal/update"
	"github.com/spf13/cobra"
)

// Version is set at build time via ldflags for release builds.
var Version = "dev"

// Binaries produced by `go install module/cmd/drive-git@v1.2.3` carry no
// ldflags, but Go records the module version in the build info. Recover it so
// those installs report a real version and take part in update checks.
//
// Only a clean release tag is accepted. Go's VCS stamping also produces
// "v0.1.0+dirty" for a working-tree build and pseudo-versions like
// "v0.1.1-0.20250831...-abc123" for untagged commits; both would parse as a
// release and start drawing upgrade notices that offer to overwrite a
// hand-built binary. Those stay "dev", which the update check ignores.
func init() {
	if Version != "dev" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	v := bi.Main.Version
	if v == "" || v == "(devel)" || strings.ContainsAny(v, "+-") {
		return
	}
	if _, ok := update.ParseVersion(v); ok {
		Version = v
	}
}

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
		newListCmd(), newStatusCmd(), newRmCmd(), newUnlockCmd(), newGCCmd(),
		newInstallHelperCmd(), newRemoteHelperCmd(), newUpdateCmd(),
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
	// Only after a successful command, so a failure is never buried under an
	// unrelated upgrade hint.
	notify()
	return 0
}

// isTerminal reports whether f is an interactive terminal. Scripts, pipes and
// the remote helper all get no upgrade notices.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// contextWithTimeout bounds the update check independently of the command.
func contextWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
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
