package storage

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// TenantPrefix is where every organization other than the default keeps its
// recordings: "tenants/<id>/" in front of the usual date-and-session key.
// The default organization's keys have no prefix, so an install that was
// upgraded finds every recording it had exactly where the database says.
const TenantPrefix = "tenants/"

// Scoped is a Store that one organization can see.
//
// Writes go under the organization's prefix. Reads refuse any key outside it,
// which matters because the key a read asks for came from a session row an
// agent reported: without the check, an agent in one organization could name
// another organization's object and have its own administrator read it. The
// default organization is scoped too, to everything that is not under
// TenantPrefix.
type Scoped struct {
	inner  Store
	prefix string
}

var _ Store = (*Scoped)(nil)

// ForTenant scopes s to one organization. The empty id and "default" are the
// default organization.
func ForTenant(s Store, tenantID string) *Scoped {
	if s == nil {
		return nil
	}
	if tenantID == "" || tenantID == "default" {
		return &Scoped{inner: s}
	}
	return &Scoped{inner: s, prefix: TenantPrefix + tenantID + "/"}
}

// Owns reports whether key belongs to this organization.
func (s *Scoped) Owns(key string) bool {
	if s.prefix == "" {
		return key != "" && !strings.HasPrefix(key, TenantPrefix)
	}
	return len(key) > len(s.prefix) && strings.HasPrefix(key, s.prefix)
}

// keyUploader is what a store must offer for Scoped to place an object under
// a prefix. Client and Dir both do.
type keyUploader interface {
	UploadKey(ctx context.Context, path, key, sessionID, chainHead string,
		startedAt time.Time) (string, error)
}

// Upload stores a recording under the organization's prefix.
func (s *Scoped) Upload(ctx context.Context, path, sessionID, chainHead string,
	startedAt time.Time) (string, error) {
	if s == nil || s.inner == nil {
		return "", ErrNotConfigured
	}
	if s.prefix == "" {
		return s.inner.Upload(ctx, path, sessionID, chainHead, startedAt)
	}
	ku, ok := s.inner.(keyUploader)
	if !ok {
		return "", fmt.Errorf("%T cannot store a recording under an organization's prefix", s.inner)
	}
	return ku.UploadKey(ctx, path, s.prefix+ObjectKeyExt(startedAt, sessionID, extOf(path)),
		sessionID, chainHead, startedAt)
}

// Get opens a recording the organization owns.
func (s *Scoped) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if s == nil || s.inner == nil {
		return nil, ErrNotConfigured
	}
	if !s.Owns(key) {
		return nil, fmt.Errorf("recording %q is not this organization's", key)
	}
	return s.inner.Get(ctx, key)
}

// List returns the organization's recordings.
func (s *Scoped) List(ctx context.Context) ([]Object, error) {
	if s == nil || s.inner == nil {
		return nil, ErrNotConfigured
	}
	all, err := s.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0:0]
	for _, o := range all {
		if s.Owns(o.Key) {
			out = append(out, o)
		}
	}
	return out, nil
}
