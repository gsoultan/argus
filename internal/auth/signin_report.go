package auth

import (
	"context"
	"fmt"
	"time"
)

// SignInReport is what the "this wasn't me" link in a sign-in email carries:
// which account, and which sign-in, so the alert it raises can describe it.
//
// It is mailed, so it is treated as public: it can be forwarded, previewed by
// a mail scanner, or read by whoever took the account. What it may do is
// therefore only what is safe to hand that person -- report the sign-in and
// end the account's sessions -- and it is single-use. It authenticates
// nothing. Its fields are named so that none of them is a field of Session,
// Ticket or the second-factor challenge: verified as any of those it carries
// no account and no expiry, and is refused.
type SignInReport struct {
	ID      string    `json:"rid"`
	Tenant  string    `json:"org,omitempty"`
	Account string    `json:"acct"`
	Purpose string    `json:"purpose"`
	Until   time.Time `json:"until"`

	// The sign-in, as the alert described it.
	At       time.Time `json:"at"`
	ClientIP string    `json:"ip,omitempty"`
	Device   string    `json:"dev,omitempty"`
	Method   string    `json:"m,omitempty"`
}

const signInReportPurpose = "sign-in-report"

// IssueSignInReport mints the link token for one sign-in.
func (s *Signer) IssueSignInReport(r SignInReport, ttl time.Duration) (string, error) {
	if r.Account == "" {
		return "", fmt.Errorf("%w: no account", ErrInvalid)
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	r.ID = id
	r.Tenant = s.tenant
	r.Purpose = signInReportPurpose
	r.Until = time.Now().UTC().Add(ttl)
	return s.sign(r)
}

// PeekSignInReport verifies a report token without spending it, so the page
// the link opens can say which sign-in it is about before anyone confirms.
func (s *Signer) PeekSignInReport(token string) (SignInReport, error) {
	var r SignInReport
	if err := s.verify(token, &r); err != nil {
		return SignInReport{}, err
	}
	if r.Purpose != signInReportPurpose || r.Account == "" || r.ID == "" {
		return SignInReport{}, fmt.Errorf("%w: not a sign-in report", ErrInvalid)
	}
	if r.Tenant != s.tenant {
		return SignInReport{}, ErrInvalid
	}
	if time.Now().After(r.Until) {
		return SignInReport{}, ErrExpired
	}
	return r, nil
}

// RedeemSignInReport verifies a report token and spends it. Spent through the
// same redemption record as tickets, so it is single-use on every replica.
func (s *Signer) RedeemSignInReport(ctx context.Context, token string) (SignInReport, error) {
	r, err := s.PeekSignInReport(token)
	if err != nil {
		return SignInReport{}, err
	}
	err = s.burn(ctx, Ticket{
		ID: r.ID, Tenant: r.Tenant, Email: r.Account, Scope: signInReportPurpose,
		Target: signInReportPurpose, ExpiresAt: r.Until,
	})
	return r, err
}
