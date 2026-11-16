package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Buying Reserved Instances and Savings Plans (cloud/buy.go says why it is
guarded the way it is).

	POST /api/v1/cloud/reservations/quote   AWS's price for a commitment now

The buy itself is an ordinary cloud change, ri-buy or sp-buy, with the
quoted total typed in confirm. It always waits for someone other than the
person who asked, in every environment, and is re-priced at each step: when
asked for (the typed total must match AWS's price then), and just before
buying (nothing is bought if the price moved since it was asked for).
*/

// maxRICount is the most instances one Reserved Instance buy covers.
const maxRICount = 100

// maxCommitment is the most dollars an hour one Savings Plan commits to.
const maxCommitment = 1000

var reFamily = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// cloudBuySpec is a buy as stored: what, and the offer it was asked at.
type cloudBuySpec struct {
	Query cloud.OfferQuery `json:"query"`
	Offer cloud.Offer      `json:"offer"`
}

// cloudQuoteView is a quote with what it means.
type cloudQuoteView struct {
	Offer cloud.Offer `json:"offer"`
	// TotalText is the total as the person types it.
	TotalText string `json:"totalText"`
	// OnDemand is the list price of the same hours over the term; zero for
	// a Savings Plan, whose discount depends on what it lands on.
	OnDemand float64 `json:"onDemand"`
	Saves    float64 `json:"saves"`
	// PaysOffMonth is the month the commitment costs less than on demand
	// would have, counting the upfront; 0 when it never does.
	PaysOffMonth int `json:"paysOffMonth"`
	// Covers is the running instances of its family in its region today.
	Covers []string `json:"covers"`
	// AlwaysApproval: nobody buys without someone else agreeing.
	AlwaysApproval bool `json:"alwaysApproval"`
}

func (a *API) registerCloudBuyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/cloud/reservations/quote", a.postCloudQuote)
}

func buyAction(kind string) cloud.Action {
	if kind == "sp" {
		return cloud.ActionSPBuy
	}
	return cloud.ActionRIBuy
}

// checkOfferQuery checks what is asked for and tidies it.
func checkOfferQuery(m cloudManaged, q *cloud.OfferQuery) error {
	if !hasRegion(m.acct, q.Region) {
		return errors.New("pick one of the account's regions")
	}
	if q.TermMonths != 12 && q.TermMonths != 36 {
		return errors.New("a term is one year or three")
	}
	if !cloud.ValidPayment(q.Payment) {
		return errors.New("pay no upfront, partial upfront or all upfront")
	}
	switch q.Kind {
	case "ri":
		q.PlanType, q.Family, q.Commitment = "", "", 0
		if !cloud.ValidSize(q.Size) {
			return errors.New("pick an instance type, like m6i.large")
		}
		if q.Count < 1 || q.Count > maxRICount {
			return fmt.Errorf("buy for 1 to %d instances", maxRICount)
		}
	case "sp":
		q.Size, q.Count = "", 0
		q.Commitment = math.Round(q.Commitment*1000) / 1000
		if q.Commitment < 0.001 || q.Commitment > maxCommitment {
			return fmt.Errorf("commit between $0.001 and $%d an hour", maxCommitment)
		}
		switch q.PlanType {
		case cloud.PlanCompute:
			q.Family = ""
		case cloud.PlanEC2Instance:
			if !reFamily.MatchString(q.Family) || len(q.Family) > 16 {
				return errors.New("an EC2 Instance Savings Plan is for one family, like m6i")
			}
		default:
			return errors.New("a Savings Plan is Compute or EC2 Instance")
		}
	default:
		return errors.New("buy a Reserved Instance (ri) or a Savings Plan (sp)")
	}
	return nil
}

// quote asks AWS for the price and works out what it means.
func (a *API) quote(ctx context.Context, m cloudManaged, actor string, q cloud.OfferQuery) (cloudQuoteView, int, error) {
	b, ok := m.manager.(cloud.Buyer)
	if !ok {
		return cloudQuoteView{}, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot buy reservations from Argus yet")
	}
	o, err := b.Quote(ctx, m.target, cloud.Actor(actor), q)
	if errors.Is(err, cloud.ErrNoOffer) {
		return cloudQuoteView{}, http.StatusNotFound, err
	}
	if err != nil {
		return cloudQuoteView{}, http.StatusBadGateway, errors.New("AWS did not give a price: " + err.Error())
	}
	v := cloudQuoteView{Offer: o, TotalText: totalText(o.Total), Covers: []string{}, AlwaysApproval: true}
	if q.Kind == "ri" {
		if list, ok := cloud.HourlyPrice(q.Size); ok {
			monthly := list * float64(q.Count) * cloud.HoursPerMonth
			v.OnDemand = math.Round(list*float64(q.Count)*cloud.TermHours(q.TermMonths)*100) / 100
			v.Saves = math.Round((v.OnDemand-o.Total)*100) / 100
			for mo := 1; mo <= q.TermMonths; mo++ {
				if monthly*float64(mo) >= o.Upfront+o.Monthly*float64(mo) {
					v.PaysOffMonth = mo
					break
				}
			}
		}
	}
	items, err := a.store.CloudResources(ctx, cloud.KindInstance)
	if err == nil {
		fam := cloud.Family(q.Size)
		if q.Kind == "sp" {
			fam = q.Family
		}
		for _, it := range items {
			r := it.Resource
			if it.AccountID != m.acct.ID || r.State != "running" {
				continue
			}
			if q.Kind == "sp" && q.PlanType == cloud.PlanCompute {
				continue
			}
			if r.Region == q.Region && cloud.Family(r.Size) == fam {
				v.Covers = append(v.Covers, r.Name)
			}
		}
		sort.Strings(v.Covers)
	}
	return v, http.StatusOK, nil
}

// totalText is a total as the person types it: dollars, whole when large.
func totalText(f float64) string {
	if f >= 100 {
		return "$" + commas(int64(math.Round(f)))
	}
	return fmt.Sprintf("$%.2f", f)
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// typedTotal reads what the person typed as dollars; ok false when it is
// not a number.
func typedTotal(s string) (float64, bool) {
	s = strings.NewReplacer("$", "", ",", "", " ", "", "USD", "", "usd", "").Replace(strings.TrimSpace(s))
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil && f >= 0
}

// sameTotal is whether a typed total is the total, as it was shown.
func sameTotal(typed string, total float64) bool {
	f, ok := typedTotal(typed)
	if !ok {
		return false
	}
	if total >= 100 {
		return math.Round(f) == math.Round(total)
	}
	return math.Abs(f-total) < 0.005
}

func (a *API) postCloudQuote(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudChanger(w, r)
	if !ok {
		return
	}
	var in struct {
		AccountID string           `json:"accountId"`
		Query     cloud.OfferQuery `json:"query"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	m, code, err := a.cloudManagedAccount(r.Context(), in.AccountID)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	if err := checkOfferQuery(m, &in.Query); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.storageMissing(m, buyAction(in.Query.Kind)); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	v, code, err := a.quote(r.Context(), m, actor, in.Query)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// planBuy prices the commitment again and checks the typed total against
// it.
func (a *API) planBuy(r *http.Request, m cloudManaged, actor string, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	var q cloud.OfferQuery
	if err := json.Unmarshal(in.Spec, &q); err != nil {
		return ch, http.StatusBadRequest, errors.New("a buy takes {kind, termMonths, payment, ...}")
	}
	q.Region = in.Region
	if (in.Action == cloud.ActionSPBuy) != (q.Kind == "sp") {
		return ch, http.StatusBadRequest, errors.New("ri-buy buys a Reserved Instance and sp-buy a Savings Plan")
	}
	if err := checkOfferQuery(m, &q); err != nil {
		return ch, http.StatusBadRequest, err
	}
	if err := a.storageMissing(m, in.Action); err != nil {
		return ch, http.StatusConflict, err
	}
	if len(ch.Reason) < 5 {
		return ch, http.StatusBadRequest, errors.New("say why: the person who approves a commitment reads it")
	}
	v, code, err := a.quote(r.Context(), m, actor, q)
	if err != nil {
		return ch, code, err
	}
	if !sameTotal(in.Confirm, v.Offer.Total) {
		return ch, http.StatusConflict, fmt.Errorf("type the total, %s, to confirm; AWS's price is checked again each time", v.TotalText)
	}
	ch.Environment = m.acct.Environment
	ch.Spec, _ = json.Marshal(cloudBuySpec{Query: q, Offer: v.Offer})
	term := map[int]string{12: "1 year", 36: "3 years"}[q.TermMonths]
	if q.Kind == "ri" {
		ch.ResourceName = fmt.Sprintf("%d × %s", q.Count, q.Size)
		ch.Summary = fmt.Sprintf("Buy a Reserved Instance for %d %s in %s, %s, %s: %s in all", q.Count, q.Size, q.Region, term,
			strings.ToLower(q.Payment), v.TotalText)
	} else {
		what := "Compute Savings Plan"
		if q.PlanType == cloud.PlanEC2Instance {
			what = "EC2 Instance Savings Plan for " + q.Family + " in " + q.Region
		}
		ch.ResourceName = what
		ch.Summary = fmt.Sprintf("Buy a %s at $%.3f an hour, %s, %s: %s in all", what, q.Commitment, term, strings.ToLower(q.Payment), v.TotalText)
	}
	if v.Offer.Upfront > 0 {
		ch.Summary += fmt.Sprintf(" (%s upfront)", totalText(v.Offer.Upfront))
	}
	return ch, http.StatusOK, nil
}

// errPriceMoved is AWS's price changing between asking and buying.
var errPriceMoved = errors.New("AWS's price changed since this was asked for")

// applyBuy prices it once more and buys only at the price asked for.
func (a *API) applyBuy(ctx context.Context, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	b, ok := m.manager.(cloud.Buyer)
	if !ok {
		return cloud.Result{}, errors.New(m.acct.Provider + " cannot buy reservations from Argus yet")
	}
	var s cloudBuySpec
	if err := json.Unmarshal(ch.Spec, &s); err != nil {
		return cloud.Result{}, err
	}
	actor := cloud.Actor(ch.RequestedBy)
	now, err := b.Quote(ctx, m.target, actor, s.Query)
	if err != nil {
		return cloud.Result{}, errors.New("AWS did not give a price just before buying, so nothing was bought: " + err.Error())
	}
	if !now.Same(s.Offer) {
		return cloud.Result{}, fmt.Errorf("%w (asked at %s, now %s); nothing was bought. Ask again at the new price",
			errPriceMoved, totalText(s.Offer.Total), totalText(now.Total))
	}
	s.Query.Token = ch.ID
	res, err := b.Buy(ctx, m.target, actor, s.Query, s.Offer)
	if err == nil && res.ResourceID != "" {
		res.Detail = "Bought " + res.ResourceID + " for " + totalText(s.Offer.Total)
	}
	return res, err
}
