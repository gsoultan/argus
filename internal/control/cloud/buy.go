package cloud

import (
	"context"
	"errors"
	"math"
)

/*
Buying a Reserved Instance or a Savings Plan: a commitment for one or three
years that AWS does not refund and Argus cannot undo. It is the most guarded
change Argus makes:

  - Remove level, the level for what cannot be undone.
  - Always a second person, in every environment; the person who asked can
    never approve it, even alone in the organization.
  - The person who asks types the total.
  - The price is asked of AWS when quoted, when asked for, and again just
    before buying; when it moved, nothing is bought.
  - Never automatic. Nothing in Argus asks for one by itself.
*/

const (
	ActionRIBuy Action = "ri-buy"
	ActionSPBuy Action = "sp-buy"
)

// IsBuy is whether an action buys a commitment.
func IsBuy(a Action) bool { return a == ActionRIBuy || a == ActionSPBuy }

// Payment options, AWS's words for both kinds.
const (
	PayNone    = "No Upfront"
	PayPartial = "Partial Upfront"
	PayAll     = "All Upfront"
)

// Savings Plan types Argus buys.
const (
	PlanCompute     = "Compute"
	PlanEC2Instance = "EC2Instance"
)

// ValidPayment is whether p is one of AWS's payment options.
func ValidPayment(p string) bool { return p == PayNone || p == PayPartial || p == PayAll }

// TermHours is the hours AWS counts in a term of months (12 or 36).
func TermHours(months int) float64 { return float64(months) / 12 * 8760 }

// OfferQuery is what to buy. Kind is ri or sp.
type OfferQuery struct {
	Kind       string `json:"kind"`
	Region     string `json:"region"`
	TermMonths int    `json:"termMonths"`
	Payment    string `json:"payment"`

	// A Reserved Instance: regional, Linux/UNIX, default tenancy, standard.
	Size  string `json:"size,omitempty"`
	Count int32  `json:"count,omitempty"`

	// A Savings Plan: Compute applies anywhere; EC2Instance to one family in
	// one region. Commitment is dollars an hour.
	PlanType   string  `json:"planType,omitempty"`
	Family     string  `json:"family,omitempty"`
	Commitment float64 `json:"commitment,omitempty"`

	// Token makes a buy idempotent where AWS takes one: the change's id.
	Token string `json:"-"`
}

// Offer is AWS's price for an OfferQuery, for the whole quantity.
type Offer struct {
	ID string `json:"id"`
	// Upfront is paid at once; Hourly every hour of the term.
	Upfront float64 `json:"upfront"`
	Hourly  float64 `json:"hourly"`
	// Total over the term, and a month of the hourly part.
	Total   float64 `json:"total"`
	Monthly float64 `json:"monthly"`
}

// Price fills Total and Monthly from Upfront and Hourly over months.
func (o Offer) Price(months int) Offer {
	o.Total = round2(o.Upfront + o.Hourly*TermHours(months))
	o.Monthly = round2(o.Hourly * HoursPerMonth)
	return o
}

// Same is whether two offers cost the same, to the cent.
func (o Offer) Same(b Offer) bool {
	return o.ID == b.ID && math.Abs(o.Upfront-b.Upfront) < 0.005 && math.Abs(o.Hourly-b.Hourly) < 0.000005
}

// ErrNoOffer is AWS having nothing for the query.
var ErrNoOffer = errors.New("AWS offers nothing for that; try another term or payment")

// Buyer is a provider that quotes and buys commitments. It is optional.
type Buyer interface {
	Quote(ctx context.Context, a Account, actor Actor, q OfferQuery) (Offer, error)
	Buy(ctx context.Context, a Account, actor Actor, q OfferQuery, o Offer) (Result, error)
}

// SPUpfront is the upfront part of a Savings Plan: all of it, half of it or
// none, as AWS splits them.
func SPUpfront(commitment float64, months int, payment string) float64 {
	switch payment {
	case PayAll:
		return round2(commitment * TermHours(months))
	case PayPartial:
		return round2(commitment * TermHours(months) / 2)
	}
	return 0
}
