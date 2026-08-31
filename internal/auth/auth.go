// Package auth implements the personal OAuth desktop flow with a cached token.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/darkharasho/drive-git-remote/internal/config"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// Scope is drive.file: the app only ever sees files it created itself. It is
// a non-sensitive scope, so a personal Google Cloud project needs no
// verification review and refresh tokens do not expire on the 7-day
// testing-mode clock.
const Scope = drive.DriveFileScope

func oauthConfig(c *config.Client, redirect string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Endpoint:     google.Endpoint,
		RedirectURL:  redirect,
		Scopes:       []string{Scope},
	}
}

// LoadToken reads the cached token.
func LoadToken() (*oauth2.Token, error) {
	p, err := config.TokenPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoToken
		}
		return nil, err
	}
	var t oauth2.Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	return &t, nil
}

// SaveToken caches the token at 0600.
func SaveToken(t *oauth2.Token) error {
	p, err := config.TokenPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// Logout removes the cached token.
func Logout() error {
	p, err := config.TokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ErrNoToken means the user has not logged in yet.
var ErrNoToken = fmt.Errorf("not logged in; run `drive-git login`")

// ExpiryHint explains the most common reason a refresh stops working.
const ExpiryHint = `Your Google login has expired. This is normal for an OAuth client whose
consent screen is still in "Testing": Google expires refresh tokens after
7 days. Publishing the app to production removes the limit, but requires a
verified app domain.

Run: drive-git login`

// IsLoginExpired reports whether an error is a dead refresh token — expired on
// the testing-mode clock, revoked, or invalidated by a password change. The
// check is on the message because the cause is wrapped by the OAuth transport
// and then again by the HTTP client.
func IsLoginExpired(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "invalid_grant") ||
		strings.Contains(s, "Token has been expired or revoked")
}

// Client returns an authenticated HTTP client, refreshing and re-caching the
// token as needed.
func Client(ctx context.Context) (*http.Client, error) {
	c, err := config.LoadClient()
	if err != nil {
		return nil, err
	}
	tok, err := LoadToken()
	if err != nil {
		return nil, err
	}
	cfg := oauthConfig(c, "")
	src := &cachingSource{src: cfg.TokenSource(ctx, tok), last: tok}
	return oauth2.NewClient(ctx, src), nil
}

// cachingSource writes refreshed tokens back to disk so a long-lived refresh
// token survives access-token rotation.
type cachingSource struct {
	src  oauth2.TokenSource
	last *oauth2.Token
}

func (s *cachingSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	if s.last == nil || t.AccessToken != s.last.AccessToken {
		s.last = t
		_ = SaveToken(t)
	}
	return t, nil
}

// Login runs the loopback desktop flow and caches the resulting token.
func Login(ctx context.Context, openBrowser bool) (string, error) {
	c, err := config.LoadClient()
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	cfg := oauthConfig(c, redirect)
	state := fmt.Sprintf("%d", time.Now().UnixNano())
	url := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorisation denied: "+e, http.StatusBadRequest)
			results <- result{err: fmt.Errorf("authorisation denied: %s", e)}
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			results <- result{err: fmt.Errorf("state mismatch in OAuth callback")}
			return
		}
		fmt.Fprint(w, "<html><body><h2>drive-git is signed in.</h2><p>You can close this tab.</p></body></html>")
		results <- result{code: q.Get("code")}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(os.Stderr, "Opening your browser to sign in.\nIf it does not open, visit:\n\n%s\n\n", url)
	if openBrowser {
		_ = open(url)
	}

	var res result
	select {
	case res = <-results:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(5 * time.Minute):
		return "", fmt.Errorf("timed out waiting for the browser callback")
	}
	if res.err != nil {
		return "", res.err
	}

	tok, err := cfg.Exchange(ctx, res.code)
	if err != nil {
		return "", fmt.Errorf("exchanging authorisation code: %w", err)
	}
	if tok.RefreshToken == "" {
		return "", fmt.Errorf("Google did not return a refresh token; revoke the app at https://myaccount.google.com/permissions and log in again")
	}
	if err := SaveToken(tok); err != nil {
		return "", err
	}
	return whoami(ctx, cfg, tok)
}

func whoami(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (string, error) {
	svc, err := drive.NewService(ctx, option.WithHTTPClient(oauth2.NewClient(ctx, cfg.TokenSource(ctx, tok))))
	if err != nil {
		return "", nil
	}
	about, err := svc.About.Get().Fields("user(emailAddress)").Do()
	if err != nil || about.User == nil {
		return "", nil
	}
	return about.User.EmailAddress, nil
}

func open(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
