// Package crypto handles optional client-side encryption of bundles before
// they leave the machine. Contents are opaque to Drive; filenames and sizes
// are not, so this is defence in depth rather than full metadata privacy.
package crypto

import (
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
)

// Identity is an age X25519 keypair loaded from disk.
type Identity struct {
	id *age.X25519Identity
}

// Recipient returns the public key string recorded in repo metadata.
func (i *Identity) Recipient() string { return i.id.Recipient().String() }

// LoadOrCreateIdentity reads the identity at path, generating one on first use.
func LoadOrCreateIdentity(path string) (*Identity, bool, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		id, err := parseIdentity(string(b))
		if err != nil {
			return nil, false, fmt.Errorf("parsing %s: %w", path, err)
		}
		return &Identity{id: id}, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, false, err
	}
	contents := fmt.Sprintf("# public key: %s\n%s\n", id.Recipient(), id)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		return nil, false, err
	}
	return &Identity{id: id}, true, nil
}

// LoadIdentity reads an existing identity, erroring if absent.
func LoadIdentity(path string) (*Identity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("encryption key %s not found; copy it from the machine that ran `drive-git init`", path)
		}
		return nil, err
	}
	id, err := parseIdentity(string(b))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &Identity{id: id}, nil
}

func parseIdentity(contents string) (*age.X25519Identity, error) {
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return age.ParseX25519Identity(line)
	}
	return nil, fmt.Errorf("no identity found in key file")
}

// Encrypt streams plaintext from r into w.
func (i *Identity) Encrypt(w io.Writer, r io.Reader) error {
	out, err := age.Encrypt(w, i.id.Recipient())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Decrypt streams ciphertext from r into w.
func (i *Identity) Decrypt(w io.Writer, r io.Reader) error {
	in, err := age.Decrypt(r, i.id)
	if err != nil {
		return fmt.Errorf("decrypting: %w (wrong key?)", err)
	}
	_, err = io.Copy(w, in)
	return err
}
