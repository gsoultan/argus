package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Against a stand-in for Microsoft: Entra's token endpoint, the VM metadata
service and Azure Resource Manager, answering the way the documentation
says the real ones do for a subscription set up by SetupCommandFor. Nothing
here has met a real subscription.
*/

const (
	testSub     = "6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90"
	otherSub    = "7c2a3d1f-9b55-4a1c-8e66-1d3b8f5e2ba1"
	homeTenant  = "11111111-1111-4111-8111-111111111111"
	otherTenant = "22222222-2222-4222-8222-222222222222"
	clientID    = "33333333-3333-4333-8333-333333333333"
	homeOID     = "44444444-4444-4444-8444-444444444444"
	otherOID    = "55555555-5555-4555-8555-555555555555"
)

// jwt is a token shaped like Entra's, with nothing signed.
func jwt(tid, oid, app string) string {
	enc := base64.RawURLEncoding
	claims, _ := json.Marshal(map[string]string{"tid": tid, "oid": oid, "appid": app})
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(claims) + ".sig"
}

type assignment struct {
	principal, role, scope, description string
}

type fakeMicrosoft struct {
	// tenant is the directory the subscription is in.
	tenant string
	mu     sync.Mutex
	// assignments are the subscription's role assignments.
	assignments []assignment
	agents      []string
	tokens      map[string]int
	lists       int
	// pageNICs splits the interface list over two pages, and evilNext
	// points the next page at another host.
	pageNICs, evilNext bool
	url                string
	// changes are the changing calls made, as "METHOD path body".
	changes []string
	// refuse answers every changing call as Azure does a caller without
	// the right.
	refuse bool
	// made are the disks and snapshots put since the start, by path, with
	// how many times each has been read: Azure is still making one the
	// first time it is asked.
	made map[string]int
	// roleActions are the actions the subscription's manage role allows.
	roleActions []string
	// nic is web-1's network interface as last written, and nicEtag its
	// etag; raceNIC changes the etag after every read, as someone else's
	// change in between would.
	nic     map[string]any
	nicEtag int
	raceNIC bool
	// vmBody is web-1 as a GET of it answers, when a test sets one, and
	// locks the resource locks on it.
	vmBody string
	locks  []string
}

func (m *fakeMicrosoft) changed() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.changes...)
}

func (m *fakeMicrosoft) set(a ...assignment) {
	m.mu.Lock()
	m.assignments = a
	m.mu.Unlock()
}

func (m *fakeMicrosoft) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tenant := r.PathValue("tenant")
		if r.Form.Get("client_secret") != "s3cret" || r.Form.Get("client_id") != clientID || r.Form.Get("scope") != armScope {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided.\r\nTrace ID: abc\r\nCorrelation ID: def"}`))
			return
		}
		oid := homeOID
		if tenant == otherTenant {
			oid = otherOID
		}
		m.mu.Lock()
		m.tokens[tenant]++
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": jwt(tenant, oid, clientID), "expires_in": 3599})
	})
	mux.HandleFunc("GET /metadata/identity/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" {
			http.Error(w, "", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": jwt(homeTenant, homeOID, clientID), "expires_on": "4102444800"})
	})
	sub := "/subscriptions/" + testSub
	// Without a token Azure says where to sign in; with one from another
	// directory it refuses.
	authed := func(f http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			m.agents = append(m.agents, r.Header.Get("User-Agent"))
			m.mu.Unlock()
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer authorization_uri="https://login.windows.net/`+m.tenant+`", error="invalid_token", error_description="The authentication failed because of missing 'Authorization' header."`)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"AuthenticationFailed","message":"Authentication failed. The 'Authorization' header is missing."}}`))
				return
			}
			if tok != jwt(m.tenant, homeOID, clientID) && tok != jwt(m.tenant, otherOID, clientID) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"InvalidAuthenticationTokenTenant","message":"The access token is from the wrong issuer."}}`))
				return
			}
			f(w, r)
		}
	}
	mux.HandleFunc("GET "+sub, authed(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"` + sub + `"}`)) }))
	mux.HandleFunc("GET "+sub+"/providers/Microsoft.Authorization/roleAssignments", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$filter") != "atScope()" || r.URL.Query().Get("api-version") != "2022-04-01" {
			http.Error(w, `{"error":{"code":"BadRequest","message":"bad filter"}}`, http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		var out []map[string]any
		for n, a := range m.assignments {
			out = append(out, map[string]any{"id": a.scope + "/providers/Microsoft.Authorization/roleAssignments/a" + string(rune('0'+n)),
				"properties": map[string]string{"principalId": a.principal, "scope": a.scope, "description": a.description,
					"roleDefinitionId": sub + "/providers/Microsoft.Authorization/roleDefinitions/" + a.role}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": out})
	}))
	mux.HandleFunc("GET "+sub+"/locations", authed(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":[{"name":"westeurope","metadata":{"regionType":"Physical"}},{"name":"eastus","metadata":{"regionType":"Physical"}},
			{"name":"europe","metadata":{"regionType":"Logical"}},{"name":"eastusstage","metadata":{"regionType":"Logical"}}]}`))
	}))
	rg := sub + "/resourceGroups/Web-RG/providers/"
	answers := map[string]string{
		"Microsoft.Compute/virtualMachines": `{"value":[
			{"id":"` + rg + `Microsoft.Compute/virtualMachines/web-1","name":"web-1","location":"eastus","zones":["2"],"tags":{"team":"payments"},
			 "properties":{"vmId":"AAAAAAAA-0000-4000-8000-000000000001","hardwareProfile":{"vmSize":"Standard_B2s"},"timeCreated":"2026-10-01T10:00:00.5Z","provisioningState":"Succeeded",
			  "storageProfile":{"imageReference":{"publisher":"Canonical","offer":"ubuntu-24_04-lts","sku":"server"},
			   "osDisk":{"name":"web-1_OsDisk","osType":"Linux","managedDisk":{"id":"` + rg + `Microsoft.Compute/disks/web-1_OsDisk"}},
			   "dataDisks":[{"lun":0,"name":"web-1-data","managedDisk":{"id":"` + strings.ToUpper(rg) + `MICROSOFT.COMPUTE/DISKS/WEB-1-DATA"}}]},
			  "networkProfile":{"networkInterfaces":[{"id":"` + rg + `Microsoft.Network/networkInterfaces/web-1-nic"}]}}},
			{"id":"` + rg + `Microsoft.Compute/virtualMachines/idle-1","name":"idle-1","location":"westeurope",
			 "properties":{"vmId":"aaaaaaaa-0000-4000-8000-000000000002","hardwareProfile":{"vmSize":"Standard_D2s_v5"},"priority":"Spot",
			  "storageProfile":{"osDisk":{"name":"idle-1_OsDisk","osType":"Windows"}},"networkProfile":{"networkInterfaces":[]}}},
			{"id":"` + rg + `Microsoft.Compute/virtualMachines/unknown-1","name":"unknown-1","location":"westeurope",
			 "properties":{"vmId":"aaaaaaaa-0000-4000-8000-000000000003","hardwareProfile":{"vmSize":"Standard_B1s"}}}]}`,
		"Microsoft.Compute/virtualMachines?statusOnly": `{"value":[
			{"id":"` + rg + `Microsoft.Compute/virtualMachines/web-1","properties":{"instanceView":{"statuses":[{"code":"ProvisioningState/succeeded"},{"code":"PowerState/running"}]}}},
			{"id":"` + rg + `Microsoft.Compute/virtualMachines/idle-1","properties":{"instanceView":{"statuses":[{"code":"PowerState/stopped"}]}}}]}`,
		"Microsoft.Compute/disks": `{"value":[
			{"id":"` + rg + `Microsoft.Compute/disks/web-1_OsDisk","name":"web-1_OsDisk","location":"eastus","sku":{"name":"Premium_LRS"},"managedBy":"` + rg + `Microsoft.Compute/virtualMachines/web-1",
			 "properties":{"diskSizeGB":30,"uniqueId":"bbbbbbbb-0000-4000-8000-000000000001","diskState":"Attached","osType":"Linux","timeCreated":"2026-10-01T10:00:00Z"}},
			{"id":"` + rg + `Microsoft.Compute/disks/web-1-data","name":"web-1-data","location":"eastus","managedBy":"` + rg + `Microsoft.Compute/virtualMachines/web-1",
			 "properties":{"diskSizeGB":128,"uniqueId":"bbbbbbbb-0000-4000-8000-000000000002"}},
			{"id":"` + rg + `Microsoft.Compute/disks/left-over","name":"left-over","location":"eastus","properties":{"diskSizeGB":200,"uniqueId":"bbbbbbbb-0000-4000-8000-000000000003","diskState":"Unattached"}}]}`,
		"Microsoft.Compute/snapshots": `{"value":[{"id":"` + rg + `Microsoft.Compute/snapshots/nightly","name":"nightly","location":"eastus","sku":{"name":"Standard_LRS"},
			"properties":{"diskSizeGB":30,"uniqueId":"cccccccc-0000-4000-8000-000000000001","provisioningState":"Succeeded","incremental":true,
			 "creationData":{"createOption":"Copy","sourceResourceId":"` + rg + `Microsoft.Compute/disks/web-1_OsDisk"}}}]}`,
		"Microsoft.Compute/images": `{"value":[{"id":"` + rg + `Microsoft.Compute/images/Base-Image","name":"Base-Image","location":"eastus",
			"properties":{"provisioningState":"Succeeded","hyperVGeneration":"V2","storageProfile":{"osDisk":{"osType":"Linux","diskSizeGB":30}}}}]}`,
		"Microsoft.Network/networkInterfaces": `{"value":[{"id":"` + rg + `Microsoft.Network/networkInterfaces/web-1-nic","name":"web-1-nic","location":"eastus",
			"properties":{"virtualMachine":{"id":"` + rg + `Microsoft.Compute/virtualMachines/web-1"},"networkSecurityGroup":{"id":"` + rg + `Microsoft.Network/networkSecurityGroups/web-nsg"},
			 "ipConfigurations":[{"id":"` + rg + `Microsoft.Network/networkInterfaces/web-1-nic/ipConfigurations/ipconfig1",
			  "properties":{"privateIPAddress":"10.0.1.5","primary":true,"subnet":{"id":"` + rg + `Microsoft.Network/virtualNetworks/main/subnets/web"},
			   "publicIPAddress":{"id":"` + rg + `Microsoft.Network/publicIPAddresses/web-1-ip"}}}]}}]}`,
		"Microsoft.Network/publicIPAddresses": `{"value":[
			{"id":"` + rg + `Microsoft.Network/publicIPAddresses/web-1-ip","name":"web-1-ip","location":"eastus","sku":{"name":"Standard"},
			 "properties":{"ipAddress":"20.1.2.3","publicIPAllocationMethod":"Static","resourceGuid":"dddddddd-0000-4000-8000-000000000001",
			  "ipConfiguration":{"id":"` + rg + `Microsoft.Network/networkInterfaces/web-1-nic/ipConfigurations/ipconfig1"}}},
			{"id":"` + rg + `Microsoft.Network/publicIPAddresses/spare","name":"spare","location":"westeurope","sku":{"name":"Standard"},
			 "properties":{"ipAddress":"20.9.9.9","publicIPAllocationMethod":"Static","resourceGuid":"dddddddd-0000-4000-8000-000000000002"}},
			{"id":"` + rg + `Microsoft.Network/publicIPAddresses/free-east","name":"free-east","location":"eastus","sku":{"name":"Standard"},"tags":{"team":"payments"},
			 "properties":{"ipAddress":"20.5.5.5","publicIPAllocationMethod":"Static","resourceGuid":"dddddddd-0000-4000-8000-000000000003"}},
			{"id":"` + rg + `Microsoft.Network/publicIPAddresses/lb-ip","name":"lb-ip","location":"eastus","sku":{"name":"Standard"},
			 "properties":{"ipAddress":"20.6.6.6","resourceGuid":"dddddddd-0000-4000-8000-000000000004",
			  "ipConfiguration":{"id":"` + rg + `Microsoft.Network/loadBalancers/web-lb/frontendIPConfigurations/front"}}}]}`,
		"Microsoft.Network/virtualNetworks": `{"value":[{"id":"` + rg + `Microsoft.Network/virtualNetworks/main","name":"main","location":"eastus",
			"properties":{"resourceGuid":"eeeeeeee-0000-4000-8000-000000000001","addressSpace":{"addressPrefixes":["10.0.0.0/16"]},
			 "subnets":[{"id":"` + rg + `Microsoft.Network/virtualNetworks/main/subnets/web","name":"web","properties":{"addressPrefix":"10.0.1.0/24",
			   "networkSecurityGroup":{"id":"` + rg + `Microsoft.Network/networkSecurityGroups/subnet-nsg"}}},
			  {"id":"` + rg + `Microsoft.Network/virtualNetworks/main/subnets/db","name":"db","properties":{"addressPrefixes":["10.0.2.0/24"]}}]}}]}`,
		"Microsoft.Network/networkSecurityGroups": `{"value":[
			{"id":"` + rg + `Microsoft.Network/networkSecurityGroups/web-nsg","name":"web-nsg","location":"eastus","properties":{"resourceGuid":"ffffffff-0000-4000-8000-000000000001","securityRules":[
			 {"name":"ssh","properties":{"priority":300,"direction":"Inbound","access":"Allow","protocol":"Tcp","sourceAddressPrefix":"*","destinationAddressPrefix":"*","destinationPortRanges":["22","8000-8080"]}},
			 {"name":"lb","properties":{"priority":310,"direction":"Inbound","access":"Allow","protocol":"*","sourceAddressPrefix":"AzureLoadBalancer","destinationAddressPrefix":"*","destinationPortRange":"*"}},
			 {"name":"office","properties":{"priority":320,"direction":"Inbound","access":"Allow","protocol":"Icmp","sourceAddressPrefixes":["203.0.113.0/24"],"destinationAddressPrefix":"*","destinationPortRange":"*","description":"ping from the office"}},
			 {"name":"no-smtp","properties":{"priority":100,"direction":"Outbound","access":"Deny","protocol":"Tcp","sourceAddressPrefix":"*","destinationAddressPrefix":"Internet","destinationPortRange":"25"}}]}},
			{"id":"` + rg + `Microsoft.Network/networkSecurityGroups/subnet-nsg","name":"subnet-nsg","location":"eastus","properties":{"resourceGuid":"ffffffff-0000-4000-8000-000000000002","securityRules":[
			 {"name":"shut","properties":{"priority":100,"direction":"Inbound","access":"Deny","protocol":"*","sourceAddressPrefix":"*","destinationAddressPrefix":"*","destinationPortRange":"*"}},
			 {"name":"rdp","properties":{"priority":200,"direction":"Inbound","access":"Allow","protocol":"Tcp","sourceAddressPrefix":"Internet","destinationAddressPrefix":"*","destinationPortRange":"3389"}},
			 {"name":"out","properties":{"priority":200,"direction":"Outbound","access":"Allow","protocol":"Tcp","sourceAddressPrefix":"*","destinationAddressPrefix":"10.9.0.0/16","destinationPortRange":"443"}}]}},
			{"id":"` + rg + `Microsoft.Network/networkSecurityGroups/unused","name":"unused","location":"westeurope","properties":{"resourceGuid":"ffffffff-0000-4000-8000-000000000003"}}]}`,
	}
	// Changes to web-1, recorded as made. Azure accepts and answers with
	// the ids its Activity log files the operation under.
	vmPath := rg + "Microsoft.Compute/virtualMachines/web-1"
	change := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.changes = append(m.changes, r.Method+" "+strings.TrimPrefix(r.URL.Path, vmPath)+" "+string(b))
		refuse := m.refuse
		m.mu.Unlock()
		if r.URL.Query().Get("api-version") == "" {
			http.Error(w, `{"error":{"code":"MissingApiVersionParameter","message":"api-version is required"}}`, http.StatusBadRequest)
			return
		}
		if refuse {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"AuthorizationFailed","message":"The client does not have authorization to perform action over scope."}}`))
			return
		}
		w.Header().Set("x-ms-correlation-request-id", "corr-1234")
		w.Header().Set("x-ms-request-id", "req-5678")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_, _ = w.Write([]byte(`{"properties":{}}`))
	}
	// Disks and snapshots: made at a name, read back while Azure makes
	// them, grown, tagged, deleted, and attached through the VM.
	held := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path := r.URL.Path
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Method != http.MethodGet {
			m.changes = append(m.changes, r.Method+" "+strings.TrimPrefix(path, rg)+" "+string(b))
			if m.refuse {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"AuthorizationFailed","message":"The client does not have authorization to perform action over scope."}}`))
				return
			}
			w.Header().Set("x-ms-correlation-request-id", "corr-1234")
		}
		switch r.Method {
		case http.MethodGet:
			n, ok := m.made[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"ResourceNotFound","message":"The Resource was not found."}}`))
				return
			}
			m.made[path] = n + 1
			state := "Creating"
			if n > 0 {
				state = "Succeeded"
			}
			_, _ = w.Write([]byte(`{"properties":{"uniqueId":"BBBBBBBB-0000-4000-8000-000000000009","provisioningState":"` + state + `"}}`))
		case http.MethodPut:
			if m.made == nil {
				m.made = map[string]int{}
			}
			m.made[path] = 0
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"properties":{"provisioningState":"Creating"}}`))
		case http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
		default:
			_, _ = w.Write([]byte(`{"properties":{}}`))
		}
	}
	for _, coll := range []string{"disks", "snapshots"} {
		for _, method := range []string{"GET", "PUT", "PATCH", "DELETE"} {
			mux.HandleFunc(method+" "+rg+"Microsoft.Compute/"+coll+"/{name}", authed(held))
		}
		mux.HandleFunc("PATCH "+rg+"Microsoft.Compute/"+coll+"/{name}/providers/Microsoft.Resources/tags/default", authed(held))
	}
	mux.HandleFunc("POST "+vmPath+"/attachDetachDataDisks", authed(change))
	// Public addresses are made and deleted like disks. web-1's network
	// interface is read whole and written back whole, under its etag.
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		mux.HandleFunc(method+" "+rg+"Microsoft.Network/publicIPAddresses/{name}", authed(held))
	}
	mux.HandleFunc("PATCH "+rg+"Microsoft.Network/publicIPAddresses/{name}/providers/Microsoft.Resources/tags/default", authed(held))
	nicPath := rg + "Microsoft.Network/networkInterfaces/web-1-nic"
	etag := func() string { return `W/"` + string(rune('a'+m.nicEtag)) + `"` }
	mux.HandleFunc("GET "+nicPath, authed(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.nic == nil {
			_ = json.Unmarshal([]byte(`{"id":"`+nicPath+`","name":"web-1-nic","location":"eastus","tags":{"keep":"me"},
				"properties":{"provisioningState":"Succeeded","macAddress":"00-0D-3A-00-00-01","enableAcceleratedNetworking":true,
				 "networkSecurityGroup":{"id":"`+rg+`Microsoft.Network/networkSecurityGroups/web-nsg"},
				 "ipConfigurations":[{"name":"ipconfig1","id":"`+nicPath+`/ipConfigurations/ipconfig1",
				  "properties":{"privateIPAddress":"10.0.1.5","privateIPAllocationMethod":"Dynamic","primary":true,
				   "subnet":{"id":"`+rg+`Microsoft.Network/virtualNetworks/main/subnets/web"},
				   "publicIPAddress":{"id":"`+rg+`Microsoft.Network/publicIPAddresses/web-1-ip"}}}]}}`), &m.nic)
		}
		m.nic["etag"] = etag()
		_ = json.NewEncoder(w).Encode(m.nic)
		if m.raceNIC {
			m.nicEtag++
		}
	}))
	mux.HandleFunc("PUT "+nicPath, authed(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Header.Get("If-Match") != etag() {
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"error":{"code":"PreconditionFailed","message":"Precondition failed."}}`))
			return
		}
		m.changes = append(m.changes, "PUT nic "+string(b))
		if m.refuse {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"AuthorizationFailed","message":"The client does not have authorization to perform action over scope."}}`))
			return
		}
		m.nic = nil
		_ = json.Unmarshal(b, &m.nic)
		m.nicEtag++
		w.Header().Set("x-ms-correlation-request-id", "corr-1234")
		_, _ = w.Write(b)
	}))
	mux.HandleFunc("GET "+sub+"/providers/Microsoft.Authorization/roleDefinitions/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		acts := m.roleActions
		m.mu.Unlock()
		if r.PathValue("id") != ManageRoleID(testSub) || acts == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"RoleDefinitionDoesNotExist","message":"The specified role definition does not exist."}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"properties": map[string]any{"permissions": []map[string]any{{"actions": acts}}}})
	}))
	for _, verb := range []string{"start", "deallocate", "restart", "powerOff"} {
		mux.HandleFunc("POST "+vmPath+"/"+verb, authed(change))
	}
	mux.HandleFunc("PATCH "+vmPath, authed(change))
	mux.HandleFunc("DELETE "+vmPath, authed(change))
	mux.HandleFunc("GET "+vmPath, authed(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		body := m.vmBody
		m.mu.Unlock()
		if body == "" || r.URL.Query().Get("$expand") != "instanceView" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"ResourceNotFound","message":"The Resource was not found."}}`))
			return
		}
		_, _ = w.Write([]byte(strings.ReplaceAll(body, "RG/", rg)))
	}))
	mux.HandleFunc("GET "+vmPath+"/providers/Microsoft.Authorization/locks", authed(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		out := []map[string]any{}
		for _, l := range m.locks {
			out = append(out, map[string]any{"name": l, "properties": map[string]string{"level": "CanNotDelete"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": out})
	}))
	mux.HandleFunc("PATCH "+vmPath+"/providers/Microsoft.Resources/tags/default", authed(change))
	mux.HandleFunc("GET "+vmPath+"/vmSizes", authed(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":[{"name":"Standard_D2s_v5"},{"name":"Standard_B2s"},{"name":"Standard_B2s"},{"name":"not a size"}]}`))
	}))
	for path, body := range answers {
		path, statusOnly := strings.CutSuffix(path, "?statusOnly")
		if statusOnly {
			continue
		}
		mux.HandleFunc("GET "+sub+"/providers/"+path, authed(func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			m.lists++
			pageNICs, evil := m.pageNICs, m.evilNext
			m.mu.Unlock()
			if r.URL.Query().Get("api-version") == "" {
				http.Error(w, `{"error":{"code":"MissingApiVersionParameter","message":"The api-version query parameter (?api-version=) is required for all requests."}}`, http.StatusBadRequest)
				return
			}
			out := body
			if r.URL.Query().Get("statusOnly") == "true" {
				out = answers[path+"?statusOnly"]
			}
			if path == "Microsoft.Network/networkInterfaces" && pageNICs {
				if r.URL.Query().Get("page") == "2" {
					_, _ = w.Write([]byte(out))
					return
				}
				next := m.url + r.URL.Path + "?api-version=" + r.URL.Query().Get("api-version") + "&page=2"
				if evil {
					next = "https://elsewhere.example/steal"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}, "nextLink": next})
				return
			}
			_, _ = w.Write([]byte(out))
		}))
	}
	return mux
}

func newFake(t *testing.T, ext string, managed bool) (*Provider, *fakeMicrosoft, cloud.Account) {
	t.Helper()
	m := &fakeMicrosoft{tenant: homeTenant, tokens: map[string]int{}}
	m.set(assignment{homeOID, ReaderRoleID, "/subscriptions/" + testSub, Description(ext)})
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	m.url = srv.URL
	opts := Options{}
	if !managed {
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte("s3cret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		opts = Options{TenantID: homeTenant, ClientID: clientID, CredentialsFile: path}
	}
	p := New(opts)
	p.ep = endpoints{metadata: srv.URL, login: srv.URL, arm: srv.URL}
	p.elsewhere = nil
	acct := cloud.Account{Provider: "azure", AccountID: testSub, RoleARN: RoleID(testSub, ReaderRoleID), ExternalID: ext}
	return p, m, acct
}

func TestVerifyFindsItsAssignmentAndNamesThePerson(t *testing.T) {
	for _, managed := range []bool{false, true} {
		p, m, acct := newFake(t, "argus-0123abcd", managed)
		base, err := p.Base(context.Background())
		if err != nil || base.Principal != clientID {
			t.Fatalf("managed=%v base: %+v %v", managed, base, err)
		}
		if managed != strings.Contains(base.Source, "managed identity") {
			t.Fatalf("the source must say where the identity is from: %q", base.Source)
		}
		id, err := p.Verify(context.Background(), acct, "alice@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(id.Regions, []string{"eastus", "westeurope"}) || !strings.Contains(id.ARN, "/roleAssignments/") || id.AccountID != testSub {
			t.Fatalf("verify: %+v", id)
		}
		if !slices.Contains(m.agents, "argus-control (for alice@example.com)") {
			t.Fatalf("the call must name the person: %v", m.agents)
		}
	}
}

// The refusals are the point: Reader held for another Argus, held without
// the command, held somewhere else, or not held, must each keep Argus out.
func TestVerifyRefusesAnAssignmentThatIsNotThisArguss(t *testing.T) {
	const ext = "argus-0123abcd"
	scope := "/subscriptions/" + testSub
	cases := []struct {
		name string
		a    []assignment
		want string
	}{
		{"none", nil, "has not been given the role"},
		{"another Argus's external id", []assignment{{homeOID, ReaderRoleID, scope, Description("argus-ffff")}}, "another Argus"},
		{"an id that only starts the same", []assignment{{homeOID, ReaderRoleID, scope, Description(ext + "99")}}, "another Argus"},
		{"no description", []assignment{{homeOID, ReaderRoleID, scope, ""}}, "not from this Argus's command"},
		{"someone else's assignment", []assignment{{otherOID, ReaderRoleID, scope, Description(ext)}}, "has not been given the role"},
		{"another role", []assignment{{homeOID, "b24988ac-6180-42a0-ab88-20f7382dd24c", scope, Description(ext)}}, "has not been given the role"},
		{"inherited from above", []assignment{{homeOID, ReaderRoleID, "/providers/Microsoft.Management/managementGroups/root", Description(ext)}}, "has not been given the role"},
		{"on one resource group", []assignment{{homeOID, ReaderRoleID, scope + "/resourceGroups/web-rg", Description(ext)}}, "has not been given the role"},
	}
	for _, c := range cases {
		p, m, acct := newFake(t, ext, false)
		m.set(c.a...)
		if _, err := p.Verify(context.Background(), acct, "alice@example.com"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want a refusal saying %q", c.name, err, c.want)
		}
		if _, err := p.Inventory(context.Background(), acct, "eastus", cloud.Kinds); err == nil {
			t.Errorf("%s: the inventory must be refused too", c.name)
		}
		if m.lists != 0 {
			t.Errorf("%s: nothing may be listed before the assignment is checked, %d lists were", c.name, m.lists)
		}
	}
	// Another organization's external id for the same subscription.
	p, _, acct := newFake(t, ext, false)
	acct.ExternalID = "argus-other-org"
	if _, err := p.Verify(context.Background(), acct, "alice@example.com"); err == nil {
		t.Fatal("an account with another external id must be refused")
	}
	acct.ExternalID = ""
	if _, err := p.Verify(context.Background(), acct, "alice@example.com"); err == nil {
		t.Fatal("an account with no external id must be refused")
	}
	// A role path in another subscription than the account's.
	p, _, acct = newFake(t, ext, false)
	acct.RoleARN = RoleID("99999999-9999-4999-8999-999999999999", ReaderRoleID)
	if _, err := p.Verify(context.Background(), acct, "alice@example.com"); err == nil {
		t.Fatal("a role in another subscription must be refused")
	}
	if _, err := p.Verify(context.Background(), cloud.Account{AccountID: testSub, RoleARN: RoleID(testSub, ReaderRoleID), ExternalID: ext}, "bad actor\n"); err == nil {
		t.Fatal("an actor that cannot be written in a header must be refused")
	}
}

func TestAnAppRegistrationFollowsTheSubscriptionToItsDirectory(t *testing.T) {
	p, m, acct := newFake(t, "argus-0123abcd", false)
	m.tenant = otherTenant
	m.set(assignment{otherOID, ReaderRoleID, "/subscriptions/" + testSub, Description("argus-0123abcd")})
	if _, err := p.Verify(context.Background(), acct, "alice@example.com"); err != nil {
		t.Fatalf("the token must be asked for in the subscription's own directory: %v", err)
	}
	if m.tokens[otherTenant] != 1 {
		t.Fatalf("tokens asked per directory: %v", m.tokens)
	}
	// The managed identity has one directory, and says so.
	p, m, acct = newFake(t, "argus-0123abcd", true)
	m.tenant = otherTenant
	_, err := p.Verify(context.Background(), acct, "alice@example.com")
	if err == nil || !strings.Contains(err.Error(), "use an app registration") {
		t.Fatalf("a managed identity cannot leave its directory, and the refusal must say what to do: %v", err)
	}
}

func TestTheIdentityThatDoesNotAnswerSaysWhy(t *testing.T) {
	p, _, _ := newFake(t, "x", false)
	if err := os.WriteFile(p.opts.CredentialsFile, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := p.Base(context.Background())
	if !errors.Is(err, cloud.ErrNoBase) || !strings.Contains(err.Error(), "AADSTS7000215") || strings.Contains(err.Error(), "Trace ID") ||
		!strings.Contains(err.Error(), "make a new one") {
		t.Fatalf("a wrong secret: %v", err)
	}
	if err := os.Chmod(p.opts.CredentialsFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Base(context.Background()); !errors.Is(err, cloud.ErrNoBase) {
		t.Fatalf("a secret others can read must be refused: %v", err)
	}
	for _, o := range []Options{{TenantID: homeTenant}, {TenantID: homeTenant, ClientID: clientID}, {CredentialsFile: "/x"},
		{TenantID: "Contoso.onmicrosoft.com", ClientID: clientID, CredentialsFile: "/x"}} {
		if _, err := New(o).Base(context.Background()); !errors.Is(err, cloud.ErrNoBase) {
			t.Errorf("%+v must be refused before anything is asked: %v", o, err)
		}
	}
}

func TestAMachineKnownNotToBeAzureIsNotAskedForAManagedIdentity(t *testing.T) {
	p, _, _ := newFake(t, "x", true)
	p.elsewhere = func() bool { return true }
	p.ep.metadata = "http://127.0.0.1:1" // nothing may be asked
	if _, err := p.Base(context.Background()); !errors.Is(err, cloud.ErrNoBase) || !strings.Contains(err.Error(), "not on an Azure VM") {
		t.Fatalf("got %v", err)
	}
}

func TestInventoryCutsOneReadIntoRegions(t *testing.T) {
	p, m, acct := newFake(t, "argus-0123abcd", false)
	m.pageNICs = true
	ctx := context.Background()
	east, err := p.Inventory(ctx, acct, "eastus", cloud.Kinds)
	if err != nil {
		t.Fatal(err)
	}
	first := m.lists
	west, err := p.Inventory(ctx, acct, "westeurope", cloud.Kinds)
	if err != nil {
		t.Fatal(err)
	}
	if m.lists != first {
		t.Fatalf("the second region must come from the same read: %d lists, then %d", first, m.lists)
	}
	if !slices.Equal(east.Kinds, Kinds) {
		t.Fatalf("kinds read: %v", east.Kinds)
	}
	find := func(s cloud.Snapshot, kind cloud.Kind, name string) cloud.Resource {
		t.Helper()
		for _, r := range s.Resources {
			if r.Kind == kind && r.Name == name {
				return r
			}
		}
		t.Fatalf("no %s named %s in %s", kind, name, s.Region)
		return cloud.Resource{}
	}
	has := func(r cloud.Resource, kind cloud.Kind, id string) bool {
		return slices.ContainsFunc(r.Links, func(l cloud.Ref) bool { return l.Kind == kind && l.ID == id })
	}

	web := find(east, cloud.KindInstance, "web-1")
	if web.ID != "aaaaaaaa-0000-4000-8000-000000000001" || web.State != "running" || web.Size != "Standard_B2s" || web.Zone != "eastus-2" ||
		!slices.Equal(web.Addresses, []string{"10.0.1.5"}) || web.PublicIP != "20.1.2.3" || web.Network != "eeeeeeee-0000-4000-8000-000000000001" ||
		web.Tags["team"] != "payments" || web.CreatedAt == nil {
		t.Fatalf("web-1: %+v", web)
	}
	if web.Details["resourceGroup"] != "Web-RG" || !strings.HasSuffix(web.Details["resourceId"], "/virtualMachines/web-1") ||
		web.Details["rootDeviceName"] != "web-1_OsDisk" || web.Details["platform"] != "linux" {
		t.Fatalf("web-1 details: %v", web.Details)
	}
	// Its interface's group and its subnet's group both apply; the data
	// disk is found though Azure wrote its path in another case.
	for _, l := range []cloud.Ref{{Kind: cloud.KindFirewall, ID: "ffffffff-0000-4000-8000-000000000001"}, {Kind: cloud.KindFirewall, ID: "ffffffff-0000-4000-8000-000000000002"},
		{Kind: cloud.KindSubnet, ID: "web-rg/main/web"}, {Kind: cloud.KindVolume, ID: "bbbbbbbb-0000-4000-8000-000000000001"}, {Kind: cloud.KindVolume, ID: "bbbbbbbb-0000-4000-8000-000000000002"}} {
		if !has(web, l.Kind, l.ID) {
			t.Errorf("web-1 must link %s %s: %+v", l.Kind, l.ID, web.Links)
		}
	}
	idle := find(west, cloud.KindInstance, "idle-1")
	if idle.State != "stopped" || idle.Details["allocated"] != "yes" || idle.Details["lifecycle"] != "spot" {
		t.Fatalf("a VM stopped but still allocated must say so: %+v", idle)
	}
	if u := find(west, cloud.KindInstance, "unknown-1"); u.State != "" {
		t.Fatalf("a VM whose power state was not read must have none said for it: %q", u.State)
	}

	if d := find(east, cloud.KindVolume, "left-over"); d.State != "available" || d.Size != "200 GiB" || len(d.Links) != 0 {
		t.Fatalf("left-over: %+v", d)
	}
	if d := find(east, cloud.KindVolume, "web-1_OsDisk"); d.State != "in-use" || !has(d, cloud.KindInstance, web.ID) || d.Details["type"] != "Premium_LRS" {
		t.Fatalf("os disk: %+v", d)
	}
	if s := find(east, cloud.KindSnapshot, "nightly"); s.State != "completed" || !has(s, cloud.KindVolume, "bbbbbbbb-0000-4000-8000-000000000001") {
		t.Fatalf("snapshot: %+v", s)
	}
	if im := find(east, cloud.KindImage, "Base-Image"); im.ID != "web-rg/base-image" || im.State != "available" || im.Size != "30 GiB" {
		t.Fatalf("image: %+v", im)
	}
	if a := find(east, cloud.KindAddress, "web-1-ip"); a.State != "attached" || a.PublicIP != "20.1.2.3" || !has(a, cloud.KindInstance, web.ID) {
		t.Fatalf("attached address: %+v", a)
	}
	if a := find(west, cloud.KindAddress, "spare"); a.State != "unattached" {
		t.Fatalf("spare address: %+v", a)
	}
	if n := find(east, cloud.KindNetwork, "main"); n.Size != "10.0.0.0/16" {
		t.Fatalf("network: %+v", n)
	}
	if s := find(east, cloud.KindSubnet, "db"); s.Size != "10.0.2.0/24" || s.Network != "eeeeeeee-0000-4000-8000-000000000001" || s.ID != "web-rg/main/db" {
		t.Fatalf("subnet: %+v", s)
	}

	nsg := find(east, cloud.KindFirewall, "web-nsg")
	want := []cloud.Rule{
		{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "0.0.0.0/0", Description: "ssh"},
		{Inbound: true, Protocol: "tcp", FromPort: 8000, ToPort: 8080, Source: "0.0.0.0/0", Description: "ssh"},
		{Inbound: true, Protocol: "all", FromPort: 0, ToPort: 65535, Source: "tag:AzureLoadBalancer", Description: "lb"},
		{Inbound: true, Protocol: "icmp", FromPort: -1, ToPort: -1, Source: "203.0.113.0/24", Description: "office: ping from the office"},
	}
	if !slices.Equal(nsg.Rules, want) {
		t.Fatalf("web-nsg rules:\n got %+v\nwant %+v", nsg.Rules, want)
	}
	if !nsg.Rules[0].Open() || nsg.Rules[2].Open() || nsg.Details["denyRules"] != "1" || nsg.Details["interfaces"] != "1" {
		t.Fatalf("web-nsg: open %v/%v, details %v", nsg.Rules[0].Open(), nsg.Rules[2].Open(), nsg.Details)
	}
	// A deny-everything ahead of an allow shuts it; the other direction is
	// its own.
	sn := find(east, cloud.KindFirewall, "subnet-nsg")
	if len(sn.Rules) != 1 || sn.Rules[0].Inbound || sn.Rules[0].Source != "10.9.0.0/16" || !has(sn, cloud.KindSubnet, "web-rg/main/web") {
		t.Fatalf("subnet-nsg: %+v", sn)
	}
	if u := find(west, cloud.KindFirewall, "unused"); u.Details["interfaces"] != "0" {
		t.Fatalf("a group on nothing must count as on nothing: %v", u.Details)
	}

	only, err := p.Inventory(ctx, acct, "eastus", []cloud.Kind{cloud.KindVolume, cloud.KindLoadBalancer})
	if err != nil || !slices.Equal(only.Kinds, []cloud.Kind{cloud.KindVolume}) {
		t.Fatalf("only the kinds asked for and read: %v %v", only.Kinds, err)
	}
	for _, r := range only.Resources {
		if r.Kind != cloud.KindVolume {
			t.Fatalf("asked for disks, got %s", r.Kind)
		}
	}
}

func TestANextPageElsewhereIsNotFollowed(t *testing.T) {
	p, m, acct := newFake(t, "argus-0123abcd", false)
	m.pageNICs, m.evilNext = true, true
	if _, err := p.Inventory(context.Background(), acct, "eastus", cloud.Kinds); err == nil || !strings.Contains(err.Error(), "another address") {
		t.Fatalf("a next link off Azure must fail the read, not carry the token there: %v", err)
	}
}

func TestSetupCommand(t *testing.T) {
	const ext = "argus-0123abcd"
	cmd := SetupCommandFor(testSub, []string{clientID, "66666666-6666-4666-8666-666666666666"}, ext, false)
	if strings.Contains(cmd, "roleDefinitions/$M") || strings.Contains(cmd, ManageRoleID(testSub)) {
		t.Fatalf("a read only command must make no role:\n%s", cmd)
	}
	both := SetupCommandFor(testSub, []string{clientID}, ext, true)
	for _, want := range []string{"M=" + ManageRoleID(testSub), `roleDefinitions/$M?api-version=2022-04-01`, `\"assignableScopes\":[\"/subscriptions/$S\"]`,
		"grant " + clientID + " $R ", "grant " + clientID + " $M " + AssignmentName(testSub, clientID, ManageRoleID(testSub), ext),
		ManageDescription(ext), `\"Microsoft.Compute/virtualMachines/restart/action\"`} {
		if !strings.Contains(both, want) {
			t.Errorf("the command with changes lacks %q:\n%s", want, both)
		}
	}
	if !ValidGUID(ManageRoleID(testSub)) || ManageRoleID(testSub) == ManageRoleID(otherSub) || ManageRoleID(testSub) == ReaderRoleID {
		t.Fatal("a manage role's id must be a GUID of its own per subscription")
	}
	if !hasMarker(ManageDescription(ext), ext) {
		t.Fatal("the manage description the command writes must be the one a change looks for")
	}
	for _, want := range []string{"S=" + testSub, ReaderRoleID, ExternalIDMarker + ext, "grant " + clientID + " ", "grant 66666666-6666-4666-8666-666666666666 ",
		"az ad sp create", "api-version=2022-04-01"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the command lacks %q:\n%s", want, cmd)
		}
	}
	n := AssignmentName(testSub, clientID, ReaderRoleID, ext)
	if !ValidGUID(n) || n != AssignmentName(testSub, clientID, ReaderRoleID, ext) || n == AssignmentName(testSub, clientID, ReaderRoleID, ext+"1") {
		t.Fatalf("an assignment's name must be a GUID, the same each time, and its own per external id: %s", n)
	}
	if !hasMarker(Description(ext), ext) {
		t.Fatal("the description the command writes must be the one Verify looks for")
	}
	if s, r, ok := ParseRoleID(RoleID(testSub, ReaderRoleID)); !ok || s != testSub || r != ReaderRoleID {
		t.Fatal("a role id must read back")
	}
	if got := pathID("/subscriptions/" + testSub + "/resourceGroups/Web/providers/Microsoft.Network/virtualNetworks/Main/subnets/DB"); got != "web/main/db" {
		t.Fatalf("pathID: %q", got)
	}
}
