package gateway

import (
	"context"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/hostkey"
	"github.com/gsoultan/argus/internal/reporter"
)

// controlPlaneRedeemer burns terminal tickets in the control plane so every
// gateway shares one redemption set.
//
// Without it each gateway keeps its own map, and a ticket burned on one stays
// valid on the others — "single use" silently becomes "single use per gateway".
type controlPlaneRedeemer struct{ client *reporter.Client }

func (r controlPlaneRedeemer) Redeem(ctx context.Context, t auth.Ticket) (bool, error) {
	return r.client.RedeemTicket(ctx, t.ID, t.Email, t.Target, t.Principal, t.ExpiresAt)
}

// NewControlPlaneRedeemer adapts a reporter client to auth.Redeemer.
func NewControlPlaneRedeemer(c *reporter.Client) auth.Redeemer {
	return controlPlaneRedeemer{client: c}
}

// controlPlanePins stores host key pins in the control plane so every gateway
// verifies a target against the same recorded identity.
type controlPlanePins struct{ client *reporter.Client }

func (p controlPlanePins) Pin(ctx context.Context, host string) (*hostkey.Pin, error) {
	got, err := p.client.HostKeyPin(ctx, host)
	if err != nil || got == nil {
		return nil, err
	}
	return &hostkey.Pin{
		Host:        got.Host,
		Fingerprint: got.Fingerprint,
		KeyType:     got.KeyType,
		PinnedAt:    got.PinnedAt,
		PinnedBy:    got.PinnedBy,
	}, nil
}

func (p controlPlanePins) Record(ctx context.Context, pin hostkey.Pin) error {
	return p.client.PinHostKey(ctx, reporter.HostKeyPin{
		Host:        pin.Host,
		Fingerprint: pin.Fingerprint,
		KeyType:     pin.KeyType,
		PinnedBy:    pin.PinnedBy,
	})
}

// NewControlPlanePins adapts a reporter client to hostkey.Remote.
func NewControlPlanePins(c *reporter.Client) hostkey.Remote {
	return controlPlanePins{client: c}
}
