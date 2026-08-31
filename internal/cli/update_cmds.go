package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/darkharasho/drive-git-remote/internal/update"
	"github.com/spf13/cobra"
)

// noticeSuppressed stops the post-command upgrade notice from firing on the
// update command itself, where it would just be noise.
var noticeSuppressed bool

func newUpdateCmd() *cobra.Command {
	var check, force bool
	cmd := &cobra.Command{
		Use:     "update",
		Aliases: []string{"upgrade"},
		Short:   "Update drive-git to the latest release",
		RunE: func(cmd *cobra.Command, args []string) error {
			noticeSuppressed = true
			ctx := cmd.Context()

			if check {
				rel, err := update.Latest(ctx)
				if err != nil {
					return err
				}
				switch {
				case update.Newer(Version, rel.TagName):
					fmt.Printf("%s is available (you have %s).\nRun `drive-git update` to upgrade.\n",
						rel.TagName, Version)
				case rel.TagName == Version:
					fmt.Printf("Up to date (%s).\n", Version)
				default:
					fmt.Printf("Latest release is %s; this build reports %s.\n", rel.TagName, Version)
				}
				return nil
			}

			res, err := update.Apply(ctx, Version, force)
			if err != nil {
				return err
			}
			if res.NoChange {
				fmt.Printf("Already up to date (%s).\n", res.From)
				return nil
			}
			fmt.Printf("Updated %s\n  %s -> %s\n", res.Path, res.From, res.To)
			fmt.Println("The git-remote-gdrive symlink points at the same file, so it updated too.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "report the latest release without installing it")
	cmd.Flags().BoolVar(&force, "force", false, "install the latest release even if it is not newer")
	return cmd
}

// notify prints the upgrade hint to stderr, so it never contaminates command
// output that a script might be parsing.
func notify() {
	if noticeSuppressed || !isTerminal(os.Stderr) {
		return
	}
	ctx, cancel := contextWithTimeout()
	defer cancel()
	if msg := update.Notice(ctx, Version, time.Now()); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
}
