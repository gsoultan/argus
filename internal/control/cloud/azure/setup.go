package azure

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Description is what the Reader assignment's description says: what it is
// for, and the external id that ties it to one Argus organization.
func Description(externalID string) string {
	return "Argus reads this subscription's inventory through this assignment. " + ExternalIDMarker + externalID
}

// AssignmentName is the name a new assignment gets: a GUID, as Azure
// requires, worked out from what the assignment is, so running the command
// twice writes the same assignment rather than a second one.
func AssignmentName(subscription, principal, role, externalID string) string {
	sum := sha256.Sum256([]byte("argus role assignment:" + subscription + ":" + principal + ":" + role + ":" + externalID))
	h := hex.EncodeToString(sum[:16])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// SetupCommandFor is one paste for Azure Cloud Shell that lets each of
// principals (application ids) read a subscription, or brings that up to
// date: the application is added to the subscription's directory when it is
// not there, and given Reader at the subscription with this Argus's
// external id in the assignment's description. Running it twice changes
// nothing the second time.
//
// Every value in it is checked by the caller to be a GUID or a hex external
// id, so nothing needs more quoting than the JSON's double quotes.
func SetupCommandFor(subscription string, principals []string, externalID string) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	w("# Lets Argus read subscription " + subscription + ": Reader lists and describes, reads no data and changes nothing.")
	w("S=" + subscription + " R=" + ReaderRoleID)
	w(`grant() { # application id, name for a new assignment`)
	w(`  O=$(az ad sp show --id "$1" --query id -o tsv 2>/dev/null || az ad sp create --id "$1" --query id -o tsv) || return 1`)
	w(`  N=$(az role assignment list --assignee "$O" --role $R --scope /subscriptions/$S --query "[?scope=='/subscriptions/$S'].name | [0]" -o tsv 2>/dev/null)`)
	w(`  for i in 1 2 3 4 5 6 7 8 9 10; do`)
	w(`    az rest --method put --url "https://management.azure.com/subscriptions/$S/providers/Microsoft.Authorization/roleAssignments/${N:-$2}?api-version=2022-04-01" \`)
	w(`      --body "{\"properties\":{\"roleDefinitionId\":\"/subscriptions/$S/providers/Microsoft.Authorization/roleDefinitions/$R\",\"principalId\":\"$O\",\"principalType\":\"ServicePrincipal\",\"description\":\"` +
		Description(externalID) + `\"}}" >/dev/null && return 0`)
	w(`    sleep 6`)
	w(`  done; return 1; }`)
	w(`az account set --subscription $S && \`)
	for _, pr := range principals {
		w(`grant ` + pr + ` ` + AssignmentName(subscription, pr, ReaderRoleID, externalID) + ` && \`)
	}
	w(`echo "Done. Argus connects by itself within a few minutes."`)
	return b.String()
}
