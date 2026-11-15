package control

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// CloudSyncInterval is how often every connected account is read. The
// describe calls it makes are free in AWS.
const CloudSyncInterval = 5 * time.Minute

// CloudSettleInterval is how often a region is read again while its last
// read found something between two states, such as a snapshot being taken
// or an instance stopping, so the console follows it to the end instead of
// showing where it was up to five minutes ago. Only those regions are read.
const CloudSettleInterval = 30 * time.Second

// cloudRegionParallel bounds how many regions of one account are read at
// once, to stay well inside the provider's request rate.
const cloudRegionParallel = 4

// RunCloudSync reads every connected account in every organization each
// interval, and in between re-reads every settle the regions where
// something is still changing, until ctx ends.
func (a *API) RunCloudSync(ctx context.Context, interval time.Duration) {
	a.runCloudSync(ctx, interval, CloudSettleInterval)
}

func (a *API) runCloudSync(ctx context.Context, interval, settle time.Duration) {
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	var due time.Time // when every account is read in full again
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		full := !time.Now().Before(due)
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			list, err := a.store.CloudAccounts(tctx)
			if err != nil {
				a.log.Error("cloud sync: cannot list accounts", "error", err)
				return
			}
			for _, c := range list {
				if ctx.Err() != nil {
					return
				}
				if full {
					a.SyncCloudAccount(tctx, c)
				} else if regions := a.settlingRegions(tctx, c); len(regions) > 0 {
					a.syncCloudRegions(tctx, c, regions)
				}
			}
		})
		if err != nil {
			a.log.Error("cloud sync", "error", err)
		}
		if full {
			due = time.Now().Add(interval)
		}
		t.Reset(min(settle, max(time.Until(due), 0)))
	}
}

// syncCloudAccountSoon reads an account in the background, on a context
// that keeps the request's organization but not its deadline.
func (a *API) syncCloudAccountSoon(ctx context.Context, c CloudAccount) {
	go a.SyncCloudAccount(context.WithoutCancel(ctx), c)
}

// SyncCloudAccount reads every region of one account and stores what it
// found. A region that fails keeps its last inventory and records why; the
// others are saved. It returns false when the account was already being
// read.
func (a *API) SyncCloudAccount(ctx context.Context, c CloudAccount) bool {
	return a.readCloudAccount(ctx, c, c.Regions, true)
}

// syncCloudRegions reads only some regions of an account, the ones still
// settling. It leaves the account's last full read, and its failures, as
// they were: the next full read records those.
func (a *API) syncCloudRegions(ctx context.Context, c CloudAccount, regions []string) bool {
	return a.readCloudAccount(ctx, c, regions, false)
}

func (a *API) readCloudAccount(ctx context.Context, c CloudAccount, regions []string, full bool) bool {
	key := tenantIDOf(ctx) + "/" + c.ID
	a.cloud.mu.Lock()
	if a.cloud.syncing == nil {
		a.cloud.syncing = map[string]bool{}
	}
	if a.cloud.syncing[key] {
		a.cloud.mu.Unlock()
		return false
	}
	a.cloud.syncing[key] = true
	a.cloud.mu.Unlock()
	defer func() {
		a.cloud.mu.Lock()
		delete(a.cloud.syncing, key)
		a.cloud.mu.Unlock()
	}()

	p, err := a.cloudProviderFor(ctx, c)
	if err != nil {
		// Never read with another identity: the account says so on its row.
		a.log.Warn("cloud sync skipped", "account", c.AccountID, "identity", c.Identity, "error", err)
		return true
	}
	ext, err := a.cloudExternalID(ctx, c.Provider, c.AccountID)
	if err != nil {
		a.log.Error("cloud sync", "account", c.AccountID, "error", err)
		return true
	}
	acct := cloud.Account{Provider: c.Provider, AccountID: c.AccountID, RoleARN: c.RoleARN, ExternalID: ext}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	var mu sync.Mutex
	errs := map[string]string{}
	settling := map[string]bool{}
	sem := make(chan struct{}, cloudRegionParallel)
	var wg sync.WaitGroup
	for _, region := range regions {
		wg.Add(1)
		sem <- struct{}{}
		go func(region string) {
			defer wg.Done()
			defer func() { <-sem }()
			snap, err := p.Inventory(ctx, acct, region, cloud.Kinds)
			if err == nil {
				err = a.store.SaveCloudSnapshot(ctx, c.ID, snap)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[region] = err.Error()
				return
			}
			for _, r := range snap.Resources {
				if r.Settling() {
					settling[region] = true
					break
				}
			}
		}(region)
	}
	wg.Wait()
	a.setSettling(key, regions, settling, errs)
	if !full {
		return true
	}
	if len(errs) > 0 {
		a.log.Warn("cloud sync: some regions were not read", "account", c.AccountID, "failed", strconv.Itoa(len(errs)))
	}
	if err := a.store.SetCloudSynced(ctx, c.ID, time.Now().UTC(), errs); err != nil {
		a.log.Error("cloud sync: cannot record the sync", "account", c.AccountID, "error", err)
	}
	a.checkCloudManageRole(ctx, p, acct, c)
	return true
}

// setSettling records which of the regions just read are still settling.
// A region that failed to read is left as it was, so a snapshot is not
// forgotten because one read failed; regions not read are left alone.
func (a *API) setSettling(key string, read []string, settling map[string]bool, failed map[string]string) {
	a.cloud.mu.Lock()
	defer a.cloud.mu.Unlock()
	keep := map[string]bool{}
	for _, r := range a.cloud.settling[key] {
		keep[r] = true
	}
	for _, r := range read {
		if _, bad := failed[r]; !bad {
			keep[r] = settling[r]
		}
	}
	var out []string
	for r, on := range keep {
		if on {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		delete(a.cloud.settling, key)
		return
	}
	sort.Strings(out)
	if a.cloud.settling == nil {
		a.cloud.settling = map[string][]string{}
	}
	a.cloud.settling[key] = out
}

// settlingRegions is the regions of an account still worth reading again
// soon, limited to the regions the account is connected in now.
func (a *API) settlingRegions(ctx context.Context, c CloudAccount) []string {
	a.cloud.mu.Lock()
	got := a.cloud.settling[tenantIDOf(ctx)+"/"+c.ID]
	a.cloud.mu.Unlock()
	var out []string
	for _, r := range got {
		if slices.Contains(c.Regions, r) {
			out = append(out, r)
		}
	}
	return out
}

// checkCloudManageRole reads what the manage role may do from its own
// policy, so an account connected before a phase added permissions says so
// before a change fails, and stops saying so once its stack is updated.
// The call is free; a failure leaves the last answer.
func (a *API) checkCloudManageRole(ctx context.Context, p cloud.Provider, read cloud.Account, c CloudAccount) {
	pc, ok := p.(cloud.PolicyChecker)
	if !ok || c.ManageRoleARN == "" {
		return
	}
	missing, err := pc.ManageMissing(ctx, read, c.ManageRoleARN)
	if err != nil {
		a.log.Warn("cloud sync: cannot read the manage role's policy", "account", c.AccountID, "error", err)
		return
	}
	if err := a.store.SetCloudManageMissing(ctx, c.ID, missing); err != nil {
		a.log.Error("cloud sync: cannot record the manage role's permissions", "account", c.AccountID, "error", err)
	}
}
