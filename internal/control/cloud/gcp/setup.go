package gcp

import (
	"strings"
)

// Description is what argus-read's description says: what it is for, and
// the external id that ties it to one Argus organization.
func Description(externalID string) string {
	return "Argus reads this project's Compute Engine inventory through this account. " + ExternalIDMarker + externalID
}

// SetupCommandFor is one paste for Cloud Shell that makes argus-read in a
// project, or brings it up to date: the account with this Argus's external
// id, read-only Compute Engine for it, and the right for each of principals
// to ask for its tokens. Running it twice changes nothing the second time.
//
// Every value in it is checked by the caller to be a project id, a service
// account email or a hex external id, so nothing needs more quoting than
// the description's double quotes.
func SetupCommandFor(project string, principals []string, externalID string) string {
	sa := AccountEmail(project, ReadAccountName)
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	w("# Makes argus-read in " + project + ": it may list and describe Compute Engine, nothing else.")
	w("P=" + project + " SA=" + sa)
	w(`retry() { for i in 1 2 3 4 5 6 7 8 9 10; do "$@" >/dev/null && return 0; sleep 6; done; return 1; }`)
	w(`gcloud services enable iam.googleapis.com iamcredentials.googleapis.com --project=$P && \`)
	w(`{ gcloud iam service-accounts describe $SA --project=$P >/dev/null 2>&1 \`)
	w(`  && gcloud iam service-accounts update $SA --project=$P --description="` + Description(externalID) + `" >/dev/null \`)
	w(`  || gcloud iam service-accounts create ` + ReadAccountName + ` --project=$P --display-name="Argus (read only)" --description="` + Description(externalID) + `"; } && \`)
	w(`retry gcloud projects add-iam-policy-binding $P --member=serviceAccount:$SA --role=roles/compute.viewer --condition=None --quiet && \`)
	w(`retry gcloud projects add-iam-policy-binding $P --member=serviceAccount:$SA --role=roles/iam.serviceAccountViewer --condition=None --quiet && \`)
	for _, pr := range principals {
		w(`retry gcloud iam service-accounts add-iam-policy-binding $SA --project=$P --member=serviceAccount:` + pr + ` --role=roles/iam.serviceAccountTokenCreator --quiet && \`)
	}
	w(`echo "Done. Argus connects by itself within a minute."`)
	return b.String()
}
