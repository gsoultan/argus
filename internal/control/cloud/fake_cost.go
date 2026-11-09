package cloud

import (
	"context"
	"time"
)

// The fake as CostReader: it bills what CostRows says, and fails with
// CostErr.

func (f *Fake) Costs(_ context.Context, a Account, now time.Time) (Costs, error) {
	f.record("costs " + a.AccountID)
	if err := f.admit(a); err != nil {
		return Costs{}, err
	}
	if f.CostErr != nil {
		return Costs{}, f.CostErr
	}
	return SumCosts(now, "USD", f.CostRows, f.CostForecast), nil
}

var _ CostReader = (*Fake)(nil)
