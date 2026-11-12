package control

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// CloudSyncInterval is how often every connected account is read. The
// describe calls it makes are free in AWS.
const CloudSyncInterval = 5 * time.Minute

// cloudRegionParallel bounds how many regions of one account are read at
// once, to stay well inside the provider's request rate.
const cloudRegionParallel = 4

// RunCloudSync reads every connected account in every organization each
// interval until ctx ends.
func (a *API) RunCloudSync(ctx context.Context, interval time.Duration) {
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
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
				a.SyncCloudAccount(tctx, c)
			}
		})
		if err != nil {
			a.log.Error("cloud sync", "error", err)
		}
		t.Reset(interval)
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
	sem := make(chan struct{}, cloudRegionParallel)
	var wg sync.WaitGroup
	for _, region := range c.Regions {
		wg.Add(1)
		sem <- struct{}{}
		go func(region string) {
			defer wg.Done()
			defer func() { <-sem }()
			snap, err := p.Inventory(ctx, acct, region, cloud.Kinds)
			if err == nil {
				err = a.store.SaveCloudSnapshot(ctx, c.ID, snap)
			}
			if err != nil {
				mu.Lock()
				errs[region] = err.Error()
				mu.Unlock()
			}
		}(region)
	}
	wg.Wait()
	if len(errs) > 0 {
		a.log.Warn("cloud sync: some regions were not read", "account", c.AccountID, "failed", strconv.Itoa(len(errs)))
	}
	if err := a.store.SetCloudSynced(ctx, c.ID, time.Now().UTC(), errs); err != nil {
		a.log.Error("cloud sync: cannot record the sync", "account", c.AccountID, "error", err)
	}
	a.checkCloudManageRole(ctx, p, acct, c)
	return true
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
