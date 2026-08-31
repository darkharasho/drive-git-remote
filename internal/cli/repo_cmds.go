package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/darkharasho/drive-git-remote/internal/config"
	"github.com/darkharasho/drive-git-remote/internal/crypto"
	"github.com/darkharasho/drive-git-remote/internal/gitx"
	"github.com/darkharasho/drive-git-remote/internal/store"
	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	var name string
	var noEncrypt bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a Drive folder for the current repo and push it",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			top, err := gitx.Toplevel(wd)
			if err != nil {
				return fmt.Errorf("not inside a git repository; run `git init` first")
			}
			g := gitx.Repo{Dir: top}
			if existing := g.Config(cfgFolderID); existing != "" {
				return fmt.Errorf("this repo is already linked to Drive folder %s", existing)
			}
			if name == "" {
				name = filepath.Base(top)
			}

			b, err := backend(ctx)
			if err != nil {
				return err
			}
			rootID, err := rootFolder(ctx, b)
			if err != nil {
				return err
			}
			if id, err := b.FindFolder(ctx, rootID, name); err != nil {
				return err
			} else if id != "" {
				return fmt.Errorf("a repo named %q already exists in Drive; pass --name to choose another", name)
			}

			meta := store.Meta{Version: 1, Name: name, Encryption: "none"}
			meta.DefaultBranch, _ = g.CurrentBranch()

			r := &store.Repo{Backend: b, Git: g}
			if !noEncrypt {
				p, err := config.KeyPath()
				if err != nil {
					return err
				}
				id, created, err := crypto.LoadOrCreateIdentity(p)
				if err != nil {
					return err
				}
				r.Ident = id
				meta.Encryption, meta.Recipient = "age", id.Recipient()
				if created {
					fmt.Printf("Generated an encryption key at %s.\n"+
						"Back this up: without it these bundles cannot be decrypted.\n\n", p)
				}
			}

			folderID, err := b.EnsureFolder(ctx, rootID, name)
			if err != nil {
				return err
			}
			if err := store.WriteMeta(ctx, b, folderID, meta); err != nil {
				return err
			}
			r.FolderID, r.Meta = folderID, meta

			if err := g.SetConfig(cfgFolderID, folderID); err != nil {
				return err
			}
			if err := g.SetConfig(cfgName, name); err != nil {
				return err
			}
			fmt.Printf("Linked %s to %s/%s.\n", top, config.RootFolderName, name)
			addRemote(g, name)

			if !g.HasCommits() {
				fmt.Println("No commits yet — commit something, then run `drive-git push`.")
				return nil
			}
			return runPush(ctx, r)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "repo name in Drive (default: directory name)")
	cmd.Flags().BoolVar(&noEncrypt, "no-encrypt", false, "store bundles unencrypted")
	return cmd
}

func newCloneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clone <name> [dir]",
		Short: "Clone a repo out of Drive",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			dir := name
			if len(args) == 2 {
				dir = args[1]
			}
			if _, err := os.Stat(dir); err == nil {
				return fmt.Errorf("%s already exists", dir)
			}

			b, err := backend(ctx)
			if err != nil {
				return err
			}
			rootID, err := rootFolder(ctx, b)
			if err != nil {
				return err
			}
			folderID, err := b.FindFolder(ctx, rootID, name)
			if err != nil {
				return err
			}
			if folderID == "" {
				return fmt.Errorf("no repo named %q in Drive (try `drive-git list`)", name)
			}
			meta, err := store.ReadMeta(ctx, b, folderID)
			if err != nil {
				return err
			}
			r := &store.Repo{Backend: b, FolderID: folderID, Meta: meta}
			if err := attachKey(r); err != nil {
				return err
			}
			if err := r.CloneInto(ctx, dir); err != nil {
				return err
			}
			if err := r.Git.SetConfig(cfgFolderID, folderID); err != nil {
				return err
			}
			if err := r.Git.SetConfig(cfgName, name); err != nil {
				return err
			}
			addRemote(r.Git, name)
			abs, _ := filepath.Abs(dir)
			fmt.Printf("Cloned %s into %s.\n", name, abs)
			return nil
		},
	}
}

func newPushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "push",
		Short: "Publish local branches and tags to Drive",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo(cmd.Context())
			if err != nil {
				return err
			}
			return runPush(cmd.Context(), r)
		},
	}
}

func runPush(ctx context.Context, r *store.Repo) error {
	res, err := r.Push(ctx)
	if err != nil {
		return err
	}
	if res.UpToDate {
		fmt.Println("Everything up to date.")
		return nil
	}
	if res.RefsOnly {
		fmt.Printf("Pushed %s (ref update, no new objects).\n", res.LinkName)
		return nil
	}
	fmt.Printf("Pushed %s (%s).\n", res.LinkName, humanBytes(res.Bytes))
	return nil
}

func newPullCmd() *cobra.Command {
	var fetchOnly bool
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Fetch from Drive and fast-forward the current branch",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			r, err := openRepo(ctx)
			if err != nil {
				return err
			}
			applied, err := r.Sync(ctx)
			if err != nil {
				return err
			}
			if applied == 0 {
				fmt.Println("Already up to date with Drive.")
			} else {
				fmt.Printf("Fetched %d update(s) into refs/drive/.\n", applied)
			}
			if fetchOnly {
				return nil
			}
			return fastForward(r)
		},
	}
	cmd.Flags().BoolVar(&fetchOnly, "fetch-only", false, "update refs/drive/* without touching the working tree")
	return cmd
}

// fastForward advances only the current branch, only by fast-forward, and only
// with a clean tree. Anything else is reported for the user to resolve with
// plain git.
func fastForward(r *store.Repo) error {
	branch, err := r.Git.CurrentBranch()
	if err != nil {
		return err
	}
	if branch == "" {
		fmt.Println("HEAD is detached; refs/drive/* updated, working tree untouched.")
		return nil
	}
	mirror, err := r.MirrorRefs()
	if err != nil {
		return err
	}
	remote, ok := mirror["refs/heads/"+branch]
	if !ok {
		fmt.Printf("Branch %q does not exist on the remote yet; nothing to merge.\n", branch)
		return nil
	}

	if !r.Git.HasCommits() {
		if err := r.Git.SetRef("refs/heads/"+branch, remote); err != nil {
			return err
		}
		if _, err := r.Git.Run("reset", "--hard", "--quiet", remote); err != nil {
			return err
		}
		fmt.Printf("Checked out %s at %s.\n", branch, short(remote))
		return nil
	}
	head, err := r.Git.Run("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head == remote {
		return nil
	}
	clean, err := r.Git.IsClean()
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("working tree has uncommitted changes; refs/drive/ is updated — commit or stash, then run `git merge --ff-only refs/drive/heads/%s`", branch)
	}
	if _, err := r.Git.Run("merge", "--ff-only", "--quiet", "refs/drive/heads/"+branch); err != nil {
		return fmt.Errorf("%s has diverged from the remote; refs/drive/ is updated — resolve with `git rebase refs/drive/heads/%s` or `git merge refs/drive/heads/%s`",
			branch, branch, branch)
	}
	fmt.Printf("Fast-forwarded %s to %s.\n", branch, short(remote))
	return nil
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List repos stored in Drive",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			b, err := backend(ctx)
			if err != nil {
				return err
			}
			rootID, err := rootFolder(ctx, b)
			if err != nil {
				return err
			}
			files, err := b.List(ctx, rootID)
			if err != nil {
				return err
			}
			var names []string
			for _, f := range files {
				if f.Folder {
					names = append(names, f.Name)
				}
			}
			if len(names) == 0 {
				fmt.Printf("No repos in %s yet.\n", config.RootFolderName)
				return nil
			}
			for _, n := range names {
				fmt.Println(n)
			}
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Compare the local repo with its Drive remote",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			r, err := openRepo(ctx)
			if err != nil {
				return err
			}
			chain, chainErr := r.Chain(ctx)
			local, err := r.LocalRefs()
			if err != nil {
				return err
			}
			mirror, err := r.MirrorRefs()
			if err != nil {
				return err
			}

			fmt.Printf("Repo:       %s\n", r.Meta.Name)
			fmt.Printf("Encryption: %s\n", r.Meta.Encryption)
			if chainErr != nil {
				fmt.Printf("Remote:     %v\n", chainErr)
			} else {
				fmt.Printf("Remote:     %d link(s), tip %s\n", len(chain), store.HeadTip(chain))
			}
			fmt.Printf("Synced:     tip %s\n", tipOf(mirror))
			fmt.Printf("Local:      tip %s\n", tipOf(local))

			if lock, err := store.ReadLock(ctx, r.Backend, r.FolderID); err == nil && lock != nil {
				fmt.Printf("Lock:       held by %s\n", lock.Describe())
			}

			switch {
			case chainErr != nil:
				// already reported
			case store.HeadTip(chain) != tipOf(mirror):
				fmt.Println("\nRemote has changes you do not have — run `drive-git pull`.")
			case tipOf(local) != tipOf(mirror):
				fmt.Println("\nYou have changes the remote does not — run `drive-git push`.")
			default:
				fmt.Println("\nIn sync.")
			}

			for _, name := range gitx.SortedNames(local) {
				if !strings.HasPrefix(name, "refs/heads/") {
					continue
				}
				branch := strings.TrimPrefix(name, "refs/heads/")
				remoteSHA, ok := mirror[name]
				if !ok {
					fmt.Printf("  %s: not on remote\n", branch)
					continue
				}
				if remoteSHA == local[name] {
					continue
				}
				fmt.Printf("  %s: local %s, remote %s\n", branch, short(local[name]), short(remoteSHA))
			}
			return nil
		},
	}
}

func newUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock",
		Short: "Force-release a stale push lock",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			r, err := openRepo(ctx)
			if err != nil {
				return err
			}
			lock, err := store.ReadLock(ctx, r.Backend, r.FolderID)
			if err != nil {
				return err
			}
			if lock == nil {
				fmt.Println("No lock held.")
				return nil
			}
			fmt.Printf("Lock held by %s.\n", lock.Describe())
			if !lock.Expired(timeNow()) && !confirm("Break it anyway?") {
				return nil
			}
			if _, err := store.BreakLock(ctx, r.Backend, r.FolderID); err != nil {
				return err
			}
			fmt.Println("Lock released.")
			return nil
		},
	}
}

func newGCCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gc",
		Short: "Compact the remote chain into a single full bundle",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			r, err := openRepo(ctx)
			if err != nil {
				return err
			}
			name, err := r.Compact(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("Compacted to %s; previous links moved to %s/.\n", name, store.ArchiveFolder)
			return nil
		},
	}
}

func timeNow() time.Time { return time.Now() }

func tipOf(refs map[string]string) string {
	if len(refs) == 0 {
		return store.RootTip
	}
	return store.TipHash(refs)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
