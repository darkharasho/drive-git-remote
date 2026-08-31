// Package config resolves on-disk locations for credentials, tokens and keys.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RootFolderName is the single Drive folder that holds one subfolder per repo.
const RootFolderName = "drive-git-remote"

// Dir returns the config directory, honouring XDG_CONFIG_HOME.
func Dir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "drive-git-remote")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func path(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// ClientPath is the user's own OAuth desktop client credentials.
func ClientPath() (string, error) { return path("client.json") }

// TokenPath is the cached OAuth token.
func TokenPath() (string, error) { return path("token.json") }

// KeyPath is the age identity used for client-side encryption.
func KeyPath() (string, error) { return path("key.age") }

// Client is an OAuth installed-app credential pair. The secret is not
// meaningfully secret for desktop clients, but we still store it 0600.
type Client struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// LoadClient reads the stored OAuth client, or reports that none is set up.
func LoadClient() (*Client, error) {
	p, err := ClientPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoClient
		}
		return nil, err
	}
	var c Client
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return nil, ErrNoClient
	}
	return &c, nil
}

// SaveClient writes the OAuth client credentials.
func SaveClient(c *Client) error {
	p, err := ClientPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// ErrNoClient means the user has not yet run through `drive-git setup`.
var ErrNoClient = fmt.Errorf("no OAuth client configured; run `drive-git setup`")
