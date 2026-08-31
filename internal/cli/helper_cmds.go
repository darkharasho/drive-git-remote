package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/darkharasho/drive-git-remote/internal/gitx"
	"github.com/darkharasho/drive-git-remote/internal/helper"
	"github.com/spf13/cobra"
)

// helperBinary is the name git looks for on PATH to resolve gdrive:// URLs.
var helperBinary = "git-remote-" + helper.URLScheme

// remoteName is the git remote that init and clone wire up.
const remoteName = "gdrive"

func newInstallHelperCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "install-helper",
		Short: "Install git-remote-" + helper.URLScheme + " so plain git commands work",
		Long: "Symlinks this binary as " + helperBinary + " on your PATH.\n" +
			"Once installed, `git clone " + helper.URLScheme + "://<name>`, `git push` and\n" +
			"`git pull` work against Drive with no wrapper command.",
		RunE: func(cmd *cobra.Command, args []string) error {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			if self, err = filepath.EvalSymlinks(self); err != nil {
				return err
			}
			if dir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				dir = filepath.Join(home, ".local", "bin")
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			target := filepath.Join(dir, helperBinary)

			if existing, err := os.Readlink(target); err == nil && existing == self {
				fmt.Printf("Already installed at %s.\n", target)
				return nil
			}
			if _, err := os.Lstat(target); err == nil {
				if !confirm(fmt.Sprintf("%s already exists. Replace it?", target)) {
					return nil
				}
				if err := os.Remove(target); err != nil {
					return err
				}
			}
			if err := os.Symlink(self, target); err != nil {
				return err
			}
			fmt.Printf("Installed %s -> %s\n", target, self)

			if _, err := exec.LookPath(helperBinary); err != nil {
				fmt.Printf("\n%s is not on your PATH yet. Add it:\n  export PATH=\"%s:$PATH\"\n", dir, dir)
			} else {
				fmt.Printf("\nReady. Try: git clone %s://<name>\n", helper.URLScheme)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to install into (default: ~/.local/bin)")
	return cmd
}

// newRemoteHelperCmd exposes the helper protocol without the symlink, for
// debugging and for `git config remote.<name>.vcs`-style wiring.
func newRemoteHelperCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "remote-helper <remote> <url>",
		Short:  "Speak the git remote helper protocol on stdin/stdout",
		Hidden: true,
		Args:   cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return helper.Run(cmd.Context(), args)
		},
	}
	return cmd
}

// addRemote wires up a `gdrive` remote pointing at the Drive repo, so plain
// git commands work once the helper is installed. A pre-existing remote of
// that name is left alone.
func addRemote(g gitx.Repo, name string) {
	if g.Config("remote."+remoteName+".url") != "" {
		return
	}
	url := helper.URLScheme + "://" + name
	if _, err := g.Run("remote", "add", remoteName, url); err != nil {
		return
	}
	fmt.Printf("Added git remote %q -> %s\n", remoteName, url)
	if _, err := exec.LookPath(helperBinary); err != nil {
		fmt.Println("Run `drive-git install-helper` to use it with plain git commands.")
	}
}
