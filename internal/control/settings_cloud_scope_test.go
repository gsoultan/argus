package control

import (
	"context"
	"testing"
)

// The cloud identity is every organization's; only the default organization,
// where the platform is run from, may see or change it.
func TestCloudIdentityIsPlatformOnly(t *testing.T) {
	if !platformScope(context.Background()) {
		t.Error("no organization on the context should be the default")
	}
	if !platformScope(WithTenant(context.Background(), &Tenant{ID: DefaultTenantID})) {
		t.Error("the default organization was refused")
	}
	if platformScope(WithTenant(context.Background(), &Tenant{ID: "a1b2c3d4e5f6"})) {
		t.Error("another organization was let in")
	}
}
