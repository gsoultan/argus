package control

import (
	"context"
	"strings"
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

// Outside the default organization there is no Cloud identities setting, so
// a missing identity must not send an admin looking for one.
func TestNoIdentityNextStepNamesWhoSetsIt(t *testing.T) {
	if got := noIdentityNextStep(context.Background()); !strings.Contains(got, "access key under Settings") {
		t.Errorf("default organization: %q", got)
	}
	other := noIdentityNextStep(WithTenant(context.Background(), &Tenant{ID: "a1b2c3d4e5f6"}))
	if !strings.Contains(other, "platform's owner") || !strings.Contains(other, "default organization") {
		t.Errorf("another organization: %q", other)
	}
}
