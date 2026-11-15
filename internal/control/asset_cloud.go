package control

import (
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
	items, err := a.store.CloudResources(ctx, cloud.KindInstance)
	if err != nil {
		a.fail(w, "asset cloud", err)
		return
	}
	if len(items) > 0 {
		assets, err := a.store.Assets(ctx)
		if err != nil {
			a.fail(w, "asset cloud", err)
			return
		}
		reported, err := a.store.AgentAddresses(ctx)
		if err != nil {
			a.fail(w, "asset cloud", err)
			return
		}
		matchServers(items, assets, reported, instances)
		for i := range items {
			if items[i].Server != nil && items[i].Server.ID == id {
				out.Instance = &items[i]
				break
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
