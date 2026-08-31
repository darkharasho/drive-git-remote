// Package drive implements the store backend against Google Drive API v3.
package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/darkharasho/drive-git-remote/internal/store"
	gdrive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const folderMime = "application/vnd.google-apps.folder"

// Backend is a store.Backend backed by Drive.
type Backend struct {
	svc *gdrive.Service
}

var _ store.Backend = (*Backend)(nil)

// New builds a Drive backend from an authenticated HTTP client.
func New(ctx context.Context, hc *http.Client) (*Backend, error) {
	svc, err := gdrive.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	return &Backend{svc: svc}, nil
}

// retry wraps a Drive call with exponential backoff for rate limits and
// transient server errors. Drive throttles chatty clients aggressively, so
// every call goes through here.
func retry(ctx context.Context, op string, fn func() error) error {
	const attempts = 6
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if !retryable(err) {
			return fmt.Errorf("%s: %w", op, err)
		}
		delay := time.Duration(math.Pow(2, float64(i))) * 500 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("%s: giving up after %d attempts: %w", op, attempts, err)
}

func retryable(err error) bool {
	var ae *googleapi.Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		case http.StatusForbidden:
			// 403 is overloaded: rate limiting is retryable, permission is not.
			for _, e := range ae.Errors {
				if strings.Contains(e.Reason, "rateLimit") || strings.Contains(e.Reason, "userRateLimit") {
					return true
				}
			}
			return false
		}
		return false
	}
	return false
}

func escape(s string) string { return strings.ReplaceAll(s, "'", `\'`) }

func (b *Backend) parentQuery(parentID string) string {
	if parentID == "" {
		return "'root' in parents"
	}
	return fmt.Sprintf("'%s' in parents", escape(parentID))
}

// FindFolder returns the ID of a child folder, or "" if absent.
func (b *Backend) FindFolder(ctx context.Context, parentID, name string) (string, error) {
	q := fmt.Sprintf("%s and name = '%s' and mimeType = '%s' and trashed = false",
		b.parentQuery(parentID), escape(name), folderMime)
	var id string
	err := retry(ctx, "finding folder "+name, func() error {
		res, err := b.svc.Files.List().Q(q).Fields("files(id,name)").PageSize(10).Context(ctx).Do()
		if err != nil {
			return err
		}
		if len(res.Files) > 0 {
			id = res.Files[0].Id
		}
		return nil
	})
	return id, err
}

// EnsureFolder finds or creates a child folder.
func (b *Backend) EnsureFolder(ctx context.Context, parentID, name string) (string, error) {
	id, err := b.FindFolder(ctx, parentID, name)
	if err != nil || id != "" {
		return id, err
	}
	f := &gdrive.File{Name: name, MimeType: folderMime}
	if parentID != "" {
		f.Parents = []string{parentID}
	}
	err = retry(ctx, "creating folder "+name, func() error {
		created, err := b.svc.Files.Create(f).Fields("id").Context(ctx).Do()
		if err != nil {
			return err
		}
		id = created.Id
		return nil
	})
	return id, err
}

// List returns the immediate children of a folder.
func (b *Backend) List(ctx context.Context, parentID string) ([]store.File, error) {
	q := fmt.Sprintf("%s and trashed = false", b.parentQuery(parentID))
	var out []store.File
	err := retry(ctx, "listing folder", func() error {
		out = nil
		call := b.svc.Files.List().Q(q).
			Fields("nextPageToken, files(id,name,size,mimeType)").
			PageSize(1000).OrderBy("name")
		return call.Pages(ctx, func(page *gdrive.FileList) error {
			for _, f := range page.Files {
				out = append(out, store.File{
					ID:     f.Id,
					Name:   f.Name,
					Size:   f.Size,
					Folder: f.MimeType == folderMime,
				})
			}
			return nil
		})
	})
	return out, err
}

// Upload creates a new file. Drive permits duplicate names, which the
// append-only chain relies on to make races detectable.
func (b *Backend) Upload(ctx context.Context, parentID, name string, r io.Reader) (store.File, error) {
	// The reader may be consumed by a failed attempt, so buffer it once.
	body, err := io.ReadAll(r)
	if err != nil {
		return store.File{}, err
	}
	var out store.File
	err = retry(ctx, "uploading "+name, func() error {
		f := &gdrive.File{Name: name, Parents: []string{parentID}}
		created, err := b.svc.Files.Create(f).
			Media(strings.NewReader(string(body)), googleapi.ContentType("application/octet-stream")).
			Fields("id,name,size").Context(ctx).Do()
		if err != nil {
			return err
		}
		out = store.File{ID: created.Id, Name: created.Name, Size: created.Size}
		return nil
	})
	return out, err
}

// Download opens a file's contents.
func (b *Backend) Download(ctx context.Context, id string) (io.ReadCloser, error) {
	var rc io.ReadCloser
	err := retry(ctx, "downloading file", func() error {
		res, err := b.svc.Files.Get(id).Context(ctx).Download()
		if err != nil {
			return err
		}
		rc = res.Body
		return nil
	})
	return rc, err
}

// Delete removes a file.
func (b *Backend) Delete(ctx context.Context, id string) error {
	return retry(ctx, "deleting file", func() error {
		return b.svc.Files.Delete(id).Context(ctx).Do()
	})
}

// Move reparents a file.
func (b *Backend) Move(ctx context.Context, id, newParentID string) error {
	var current string
	if err := retry(ctx, "reading parents", func() error {
		f, err := b.svc.Files.Get(id).Fields("parents").Context(ctx).Do()
		if err != nil {
			return err
		}
		current = strings.Join(f.Parents, ",")
		return nil
	}); err != nil {
		return err
	}
	return retry(ctx, "moving file", func() error {
		_, err := b.svc.Files.Update(id, nil).
			AddParents(newParentID).RemoveParents(current).
			Fields("id").Context(ctx).Do()
		return err
	})
}
