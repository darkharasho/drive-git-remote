// Package helper implements the git remote helper protocol, so that
// `git clone gdrive://scripts` and plain `git push`/`git pull` work against a
// Drive folder with no wrapper command.
//
// Git speaks a line protocol on the helper's stdin/stdout. We advertise the
// fetch and push capabilities, which means git delegates object transfer to us
// and keeps ref bookkeeping (refspecs, remote-tracking refs, FETCH_HEAD) to
// itself. See gitremote-helpers(7).
package helper

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/darkharasho/drive-git-remote/internal/gitx"
	"github.com/darkharasho/drive-git-remote/internal/session"
	"github.com/darkharasho/drive-git-remote/internal/store"
)

// URLScheme is the transport name; the binary must be reachable on PATH as
// git-remote-<URLScheme> for `gdrive://` URLs to resolve.
const URLScheme = "gdrive"

type helper struct {
	repoName string
	repo     *store.Repo
	out      *bufio.Writer
	errw     io.Writer

	synced  bool
	dryRun  bool
	connect func(name string) (*store.Repo, error)
}

// ParseURL extracts the Drive repo name from a remote URL. Both `gdrive://x`
// and the transport-prefixed `gdrive::x` form are accepted; git strips the
// latter before invoking us, so it may arrive as a bare name.
func ParseURL(raw string) (string, error) {
	name := raw
	name = strings.TrimPrefix(name, URLScheme+"://")
	name = strings.TrimPrefix(name, URLScheme+"::")
	name = strings.Trim(name, "/")
	if name == "" {
		return "", fmt.Errorf("no repo name in remote URL %q; expected %s://<name>", raw, URLScheme)
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("repo name %q must not contain a slash; repos live directly under the Drive root folder", name)
	}
	return name, nil
}

// Run executes the helper protocol. args is os.Args[1:], which git populates
// with the remote name and URL (or just the URL for an anonymous remote).
func Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: git-remote-%s <remote> <url>", URLScheme)
	}
	raw := args[len(args)-1]
	name, err := ParseURL(raw)
	if err != nil {
		return err
	}
	h := &helper{
		repoName: name,
		out:      bufio.NewWriter(os.Stdout),
		errw:     os.Stderr,
		connect: func(name string) (*store.Repo, error) {
			// Git sets GIT_DIR, so an inherited working directory resolves to
			// the repo git is operating on.
			return session.OpenByName(ctx, name, gitx.Repo{})
		},
	}
	return h.serve(ctx, os.Stdin)
}

func (h *helper) serve(ctx context.Context, in io.Reader) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		switch {
		case line == "":
			// A blank line at command level ends the conversation.
			return h.flush()
		case line == "capabilities":
			h.reply("fetch", "push", "option")
		case line == "list" || line == "list for-push":
			if err := h.doList(ctx); err != nil {
				return err
			}
		case strings.HasPrefix(line, "option "):
			h.doOption(strings.TrimPrefix(line, "option "))
		case strings.HasPrefix(line, "fetch "):
			if err := h.doFetch(ctx, h.batch(scanner, line, "fetch ")); err != nil {
				return err
			}
		case strings.HasPrefix(line, "push "):
			if err := h.doPush(ctx, h.batch(scanner, line, "push ")); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unrecognised helper command %q", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return h.flush()
}

// batch collects a run of same-kind commands, which git terminates with a
// blank line, and returns their arguments.
func (h *helper) batch(scanner *bufio.Scanner, first, prefix string) []string {
	args := []string{strings.TrimPrefix(first, prefix)}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if line == "" {
			break
		}
		args = append(args, strings.TrimPrefix(line, prefix))
	}
	return args
}

// reply writes lines followed by the blank line that ends a response.
func (h *helper) reply(lines ...string) {
	for _, l := range lines {
		fmt.Fprintln(h.out, l)
	}
	fmt.Fprintln(h.out)
	h.out.Flush()
}

func (h *helper) flush() error { return h.out.Flush() }

func (h *helper) doOption(arg string) {
	name, value, _ := strings.Cut(arg, " ")
	switch name {
	case "dry-run":
		h.dryRun = value == "true"
		h.reply1("ok")
	case "verbosity", "progress":
		// Accepted and ignored: our progress reporting is git's own.
		h.reply1("ok")
	default:
		h.reply1("unsupported")
	}
}

// reply1 writes a single-line response, which options use instead of the
// blank-line-terminated form.
func (h *helper) reply1(line string) {
	fmt.Fprintln(h.out, line)
	h.out.Flush()
}

func (h *helper) open(ctx context.Context) error {
	if h.repo != nil {
		return nil
	}
	r, err := h.connect(h.repoName)
	if err != nil {
		return err
	}
	h.repo = r
	return nil
}

// sync applies every remote link the local repo has not seen. It runs during
// `list`, because git always lists before fetching and the ref values we must
// report only exist once the chain has been replayed.
func (h *helper) sync(ctx context.Context) error {
	if err := h.open(ctx); err != nil {
		return err
	}
	if h.synced {
		return nil
	}
	if _, err := h.repo.Sync(ctx); err != nil {
		return err
	}
	h.synced = true
	return nil
}

func (h *helper) doList(ctx context.Context) error {
	if err := h.sync(ctx); err != nil {
		return err
	}
	refs, err := h.repo.MirrorRefs()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)

	var lines []string
	for _, n := range names {
		lines = append(lines, fmt.Sprintf("%s %s", refs[n], n))
	}
	if head := h.headRef(refs); head != "" {
		lines = append(lines, "@"+head+" HEAD")
	}
	h.reply(lines...)
	return nil
}

// headRef picks the branch HEAD should track, so a clone checks out something
// sensible.
func (h *helper) headRef(refs map[string]string) string {
	for _, candidate := range []string{h.repo.Meta.DefaultBranch, "main", "master"} {
		if candidate == "" {
			continue
		}
		name := "refs/heads/" + candidate
		if _, ok := refs[name]; ok {
			return name
		}
	}
	names := make([]string, 0, len(refs))
	for n := range refs {
		if strings.HasPrefix(n, "refs/heads/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// doFetch makes the requested objects available. The chain was already
// replayed during list, and unbundling writes objects straight into the
// repository git is running against, so there is nothing left to transfer.
func (h *helper) doFetch(ctx context.Context, _ []string) error {
	if err := h.sync(ctx); err != nil {
		return err
	}
	h.reply()
	return nil
}

// pushSpec is one parsed refspec from a push batch.
type pushSpec struct {
	src   string // empty for a deletion
	dst   string
	force bool
}

func parsePushSpec(arg string) pushSpec {
	var s pushSpec
	if strings.HasPrefix(arg, "+") {
		s.force = true
		arg = arg[1:]
	}
	s.src, s.dst, _ = strings.Cut(arg, ":")
	return s
}

func (h *helper) doPush(ctx context.Context, args []string) error {
	specs := make([]pushSpec, 0, len(args))
	for _, a := range args {
		specs = append(specs, parsePushSpec(a))
	}

	// Refresh before deciding: the fast-forward checks below are only
	// meaningful against the current remote state.
	if err := h.sync(ctx); err != nil {
		return h.pushFailed(specs, err)
	}
	mirror, err := h.repo.MirrorRefs()
	if err != nil {
		return h.pushFailed(specs, err)
	}

	target := make(map[string]string, len(mirror))
	for k, v := range mirror {
		target[k] = v
	}
	results := make([]string, 0, len(specs))
	rejected := map[string]bool{}

	for _, s := range specs {
		if s.dst == "" {
			results = append(results, "error unknown malformed refspec")
			continue
		}
		if s.src == "" {
			delete(target, s.dst)
			continue
		}
		// The ref's own object, not the peeled commit: an annotated tag must
		// publish the tag object.
		sha, err := h.repo.Git.Run("rev-parse", "--verify", "--quiet", s.src)
		if err != nil {
			results = append(results, fmt.Sprintf("error %s cannot resolve %s", s.dst, s.src))
			rejected[s.dst] = true
			continue
		}
		if old, exists := target[s.dst]; exists && !s.force && old != sha {
			if !h.repo.Git.RunOK("merge-base", "--is-ancestor", old, sha) {
				results = append(results, fmt.Sprintf("error %s non-fast-forward", s.dst))
				rejected[s.dst] = true
				continue
			}
		}
		target[s.dst] = sha
	}

	if h.dryRun {
		return h.pushReport(specs, rejected, results, nil)
	}
	if store.TipHash(target) != store.TipHash(mirror) {
		if _, err := h.repo.PushRefs(ctx, target); err != nil {
			return h.pushFailed(specs, err)
		}
	}
	return h.pushReport(specs, rejected, results, nil)
}

// pushReport emits one status line per refspec.
func (h *helper) pushReport(specs []pushSpec, rejected map[string]bool, errs []string, _ error) error {
	lines := append([]string{}, errs...)
	for _, s := range specs {
		if s.dst == "" || rejected[s.dst] {
			continue
		}
		lines = append(lines, "ok "+s.dst)
	}
	h.reply(lines...)
	return nil
}

// pushFailed reports a whole-batch failure, e.g. the remote moved or the lock
// is held. Git surfaces the message next to the rejected ref.
func (h *helper) pushFailed(specs []pushSpec, cause error) error {
	msg := strings.ReplaceAll(cause.Error(), "\n", " ")
	lines := make([]string, 0, len(specs))
	for _, s := range specs {
		if s.dst == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("error %s %s", s.dst, msg))
	}
	h.reply(lines...)
	return nil
}
