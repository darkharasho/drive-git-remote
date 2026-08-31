// Command drive-git uses a private Google Drive folder as a git remote.
//
// The same binary also serves as git's remote helper: symlinked or copied to
// git-remote-gdrive on PATH, it speaks the helper protocol instead of the CLI,
// which keeps the project a single distributable binary.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darkharasho/drive-git-remote/internal/auth"
	"github.com/darkharasho/drive-git-remote/internal/cli"
	"github.com/darkharasho/drive-git-remote/internal/helper"
)

func main() {
	if invokedAsHelper() {
		if err := helper.Run(context.Background(), os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "git-remote-"+helper.URLScheme+":", err)
			if auth.IsLoginExpired(err) {
				fmt.Fprintln(os.Stderr, "\n"+auth.ExpiryHint)
			}
			os.Exit(1)
		}
		return
	}
	os.Exit(cli.Execute())
}

func invokedAsHelper() bool {
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, ".exe")
	return base == "git-remote-"+helper.URLScheme
}
