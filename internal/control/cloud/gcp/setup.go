package gcp

import (
	"strings"
)

// Description is what argus-read's description says: what it is for, and
// the external id that ties it to one Argus organization.
func Description(externalID string) string {
	return "Argus reads this project's Compute Engine inventory through this account. " + ExternalIDMarker + externalID
}

// ManageDescription is argus-manage's description, with the same external
// id as argus-read's.
func ManageDescription(externalID string) string {
	return "Argus makes the Compute Engine changes people ask for in Argus through this account. " + ExternalIDMarker + externalID
}

// ManageRoles are the roles argus-manage holds in its project. Compute
// Engine's instance administrator covers instances and disks; viewer lets
// Argus check it the way it checks argus-read; service account viewer
// lets it read its own description. Launching instances that run as a
// service account would also need Service Account User, which is left out
// until Argus launches in Google Cloud.
var ManageRoles = []string{"roles/compute.instanceAdmin.v1", "roles/compute.viewer", "roles/iam.serviceAccountViewer"}

// SetupCommandFor is one paste for Cloud Shell that makes argus-read in a
// project, or brings it up to date: the account with this Argus's external
// id, read-only Compute Engine for it, and the right for each of principals
// to ask for its tokens. Running it twice changes nothing the second time.
//
// Every value in it is checked by the caller to be a project id, a service
// account email or a hex external id, so nothing needs more quoting than
// the description's double quotes.
//
// With manage, it makes argus-manage too: the account changes go through,
// with Compute Engine's instance administrator role and nothing outside
// Compute Engine. Without it, an argus-manage already there is left alone;
// making the project read only again is done in Argus, and the account
// can be deleted in the project by hand.
func SetupCommandFor(project string, principals []string, externalID string, manage bool) string {
	sa := AccountEmail(project, ReadAccountName)
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	w("# Makes argus-read in " + project + ": it may list and describe Compute Engine, nothing else.")
	if manage {
		w("# And argus-manage: Argus makes the Compute Engine changes people ask for in Argus (start, stop, grow disks) through it.")
		w("P=" + project + " SA=" + sa + " M=" + AccountEmail(project, ManageAccountName))
	} else {
		w("P=" + project + " SA=" + sa)
	}
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
	if manage {
		w(`{ gcloud iam service-accounts describe $M --project=$P >/dev/null 2>&1 \`)
		w(`  && gcloud iam service-accounts update $M --project=$P --description="` + ManageDescription(externalID) + `" >/dev/null \`)
		w(`  || gcloud iam service-accounts create ` + ManageAccountName + ` --project=$P --display-name="Argus (changes)" --description="` + ManageDescription(externalID) + `"; } && \`)
		for _, role := range ManageRoles {
			w(`retry gcloud projects add-iam-policy-binding $P --member=serviceAccount:$M --role=` + role + ` --condition=None --quiet && \`)
		}
		for _, pr := range principals {
			w(`retry gcloud iam service-accounts add-iam-policy-binding $M --project=$P --member=serviceAccount:` + pr + ` --role=roles/iam.serviceAccountTokenCreator --quiet && \`)
		}
	}
	w(`echo "Done. Argus connects by itself within a minute."`)
	return b.String()
}
