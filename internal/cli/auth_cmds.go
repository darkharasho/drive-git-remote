package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/darkharasho/drive-git-remote/internal/auth"
	"github.com/darkharasho/drive-git-remote/internal/config"
	"github.com/spf13/cobra"
)

const setupWalkthrough = `drive-git talks to your personal Drive using an OAuth client you own, so
no credentials are shared and nothing touches an org project.

One-time setup, about two minutes:

  1. Open https://console.cloud.google.com/projectcreate and create a project
     (any name; "drive-git" is fine). Use your personal Google account.
  2. Enable the Drive API:
     https://console.cloud.google.com/apis/library/drive.googleapis.com
  3. Configure the consent screen at
     https://console.cloud.google.com/auth/overview
     Pick "External". Only three fields are required: app name, user support
     email, and developer contact email. Leave the app domain, logo, privacy
     policy and terms links blank.
  4. Under Audience, add your own Google account as a test user.
  5. Create credentials at
     https://console.cloud.google.com/apis/credentials
     -> Create credentials -> OAuth client ID -> Application type "Desktop app".
  6. Copy the client ID and client secret below.

drive-git only requests the drive.file scope, which grants access to files
this tool creates and nothing else in your Drive. The "unverified app"
warning during sign-in is expected — it is your own client.

Leave the app in "Testing" rather than publishing it. Publishing an External
app requires a verified app domain, which is far more hassle than the one
thing testing mode costs you: Google expires refresh tokens after 7 days, so
you will need to re-run "drive-git login" about weekly. drive-git tells you
plainly when that happens.

`

func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Walk through creating your own Google OAuth client",
		RunE: func(cmd *cobra.Command, args []string) error {
			if existing, err := config.LoadClient(); err == nil {
				fmt.Printf("An OAuth client is already configured (client ID %s).\n", existing.ClientID)
				if !confirm("Replace it?") {
					return nil
				}
			}
			fmt.Print(setupWalkthrough)
			id := prompt("Client ID: ")
			secret := prompt("Client secret: ")
			if id == "" || secret == "" {
				return fmt.Errorf("both client ID and secret are required")
			}
			if err := config.SaveClient(&config.Client{ClientID: id, ClientSecret: secret}); err != nil {
				return err
			}
			p, _ := config.ClientPath()
			fmt.Printf("\nSaved to %s.\nNow run `drive-git login`.\n", p)
			return nil
		},
	}
}

func newLoginCmd() *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to your personal Google account",
		RunE: func(cmd *cobra.Command, args []string) error {
			email, err := auth.Login(cmd.Context(), !noBrowser)
			if err != nil {
				return err
			}
			if email != "" {
				fmt.Printf("Signed in as %s.\n", email)
			} else {
				fmt.Println("Signed in.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the URL instead of opening a browser")
	return cmd
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the cached token",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := auth.Logout(); err != nil {
				return err
			}
			fmt.Println("Signed out. Your OAuth client and encryption key are untouched.")
			return nil
		},
	}
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the signed-in account and config locations",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, _ := config.Dir()
			fmt.Printf("Config:  %s\n", dir)
			if _, err := config.LoadClient(); err != nil {
				fmt.Println("Client:  not configured (run `drive-git setup`)")
				return nil
			}
			fmt.Println("Client:  configured")
			if _, err := auth.LoadToken(); err != nil {
				fmt.Println("Token:   not signed in (run `drive-git login`)")
				return nil
			}
			b, err := backend(cmd.Context())
			if err != nil {
				return err
			}
			id, err := rootFolder(cmd.Context(), b)
			if err != nil {
				return err
			}
			fmt.Printf("Token:   valid\nFolder:  %s (%s)\n", config.RootFolderName, id)
			return nil
		},
	}
}

func prompt(label string) string {
	fmt.Print(label)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

func confirm(label string) bool {
	answer := strings.ToLower(prompt(label + " [y/N] "))
	return answer == "y" || answer == "yes"
}
