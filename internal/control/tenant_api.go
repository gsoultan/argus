package control

import (
	"context"

	"github.com/gsoultan/argus/internal/storage"
)

// tenantSummary is what the console is told about the organization it is
// in: enough to name it, nothing that would place it among others.
func tenantSummary(ctx context.Context, t *Tenant) map[string]any {
	if t == nil {
		t = defaultTenant()
	}
	return map[string]any{"id": t.ID, "slug": t.Slug, "name": t.Name, "default": t.IsDefault()}
}

// recordingsFor is the recording store as ctx's organization may see it: its
// own prefix for writes, and a refusal for any key outside it.
func (a *API) recordingsFor(ctx context.Context) storage.Store {
	if a.recordings == nil {
		return nil
	}
	return storage.ForTenant(a.recordings, tenantIDOf(ctx))
}
