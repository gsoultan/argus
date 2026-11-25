package control

import (
	"context"
)

// tenantSummary is what the console is told about the organization it is
// in: enough to name it, nothing that would place it among others.
func tenantSummary(ctx context.Context, t *Tenant) map[string]any {
	if t == nil {
		t = defaultTenant()
	}
	return map[string]any{"id": t.ID, "slug": t.Slug, "name": t.Name, "default": t.IsDefault()}
}
