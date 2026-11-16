package control

import (
	"context"
	"net/http"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
A server's cloud side, for the Cloud card on its Overview tab: the instance
it runs as, in a cloud account connected to Argus, and how Argus knows.

The link is the one Cloud › Instances draws (matchServers): what the agent
read from the cloud's metadata service first, then address, then name. The
card says which, so a guess is never shown as a fact. When the agent names an
instance in an account Argus does not read, the card says that instead of
showing nothing.

Fleet roles only, like the System card and Cloud itself.
*/

// AssetCloud is what the Cloud card shows.
type AssetCloud struct {
	// Reported is the instance the agent says this is; nil from an agent off
	// a cloud, or older than the report.
	Reported *HostCloud `json:"reported"`
	// Instance is the instance in a connected account, as the last sync read
	// it; nil when none matched.
	Instance *CloudItem `json:"instance"`
}

func (a *API) getAssetCloud(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := a.store.AssetByID(ctx, id); err != nil {
		a.failAsset(w, "asset cloud", err)
		return
	}
	instances, err := a.store.AgentInstances(ctx)
	if err != nil {
		a.fail(w, "asset cloud", err)
		return
	}
	out := AssetCloud{}
	if c, ok := instances[id]; ok {
		out.Reported = &c
	}
	out.Instance, err = a.assetInstance(ctx, id, instances)
	if err != nil {
		a.fail(w, "asset cloud", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// assetInstance is the instance in a connected account that a server runs
// as, matched the way Cloud › Instances matches it; nil when none does.
// instances is what agents reported (AgentInstances), read once by the
// caller.
func (a *API) assetInstance(ctx context.Context, id string, instances map[string]HostCloud) (*CloudItem, error) {
	items, err := a.store.CloudResources(ctx, cloud.KindInstance)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	assets, err := a.store.Assets(ctx)
	if err != nil {
		return nil, err
	}
	reported, err := a.store.AgentAddresses(ctx)
	if err != nil {
		return nil, err
	}
	matchServers(items, assets, reported, instances)
	for i := range items {
		if items[i].Server != nil && items[i].Server.ID == id {
			return &items[i], nil
		}
	}
	return nil, nil
}
