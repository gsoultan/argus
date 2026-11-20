package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadAWSInstanceUsesIMDSv2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte("tok"))
		case r.URL.Path == "/latest/dynamic/instance-identity/document":
			if r.Header.Get("X-aws-ec2-metadata-token") != "tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"accountId":"412788931204","region":"ap-southeast-3","availabilityZone":"ap-southeast-3a",
				"instanceId":"i-0a41c27b9e3f1d27d","instanceType":"m6i.large","privateIp":"10.0.0.1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	old := awsMetadataBase
	awsMetadataBase = srv.URL
	defer func() { awsMetadataBase = old }()

	c := readAWSInstance(context.Background(), metadataClient())
	if c == nil || !c.valid() {
		t.Fatalf("got %+v", c)
	}
	want := CloudInstance{Provider: "aws", Account: "412788931204", Region: "ap-southeast-3", Zone: "ap-southeast-3a", ID: "i-0a41c27b9e3f1d27d", Type: "m6i.large"}
	if *c != want {
		t.Fatalf("got %+v, want %+v", *c, want)
	}
}

func TestReadGCPInstance(t *testing.T) {
	vals := map[string]string{
		"/computeMetadata/v1/instance/id":           "4518829021734658123",
		"/computeMetadata/v1/instance/zone":         "projects/1234/zones/asia-southeast2-a",
		"/computeMetadata/v1/instance/machine-type": "projects/1234/machineTypes/e2-standard-4",
		"/computeMetadata/v1/project/project-id":    "data-platform-prod",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		v, ok := vals[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(v))
	}))
	defer srv.Close()
	old := gcpMetadataBase
	gcpMetadataBase = srv.URL
	defer func() { gcpMetadataBase = old }()

	c := readGCPInstance(context.Background(), metadataClient())
	want := CloudInstance{Provider: "gcp", Account: "data-platform-prod", Region: "asia-southeast2", Zone: "asia-southeast2-a", ID: "4518829021734658123", Type: "e2-standard-4"}
	if c == nil || *c != want || !c.valid() {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestDiscoverCloudOnlyOnACloud(t *testing.T) {
	// Off a cloud nothing is asked: a server in a rack must not wait on a
	// link-local address.
	// A Hyper-V guest whose firmware does not carry Azure's tag is in
	// someone's own rack.
	stubAzure(t, "http://127.0.0.1:1", false, true)
	for _, hv := range []string{"", "KVM", "VMware", "Hyper-V"} {
		if c := discoverCloud(hv); c != nil {
			t.Fatalf("%q: got %+v", hv, c)
		}
	}
}

// stubAzure points the Azure reader at base and says what the firmware
// says, and forgets what an earlier test learned.
func stubAzure(t *testing.T, base string, azure, known bool) {
	t.Helper()
	oldBase, oldHW := azureMetadataBase, azureHardware
	azureMetadataBase = base
	azureHardware = func() (bool, bool) { return azure, known }
	cloudMu.Lock()
	cloudCached, azureAskedAt = nil, time.Time{}
	cloudMu.Unlock()
	t.Cleanup(func() {
		azureMetadataBase, azureHardware = oldBase, oldHW
		cloudMu.Lock()
		cloudCached, azureAskedAt = nil, time.Time{}
		cloudMu.Unlock()
	})
}

func azureMetadata(t *testing.T, asked *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*asked++
		if r.Header.Get("Metadata") != "true" || r.URL.Path != "/metadata/instance/compute" || r.URL.Query().Get("api-version") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"subscriptionId":"6B1F2C0E-8A44-4F0B-9D55-0C2A7E4D1A90","location":"eastus","zone":"2",
			"vmId":"AAAAAAAA-0000-4000-8000-000000000001","vmSize":"Standard_B2s","resourceGroupName":"Web-RG","name":"web-1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDiscoverAzureInstance(t *testing.T) {
	asked := 0
	srv := azureMetadata(t, &asked)
	stubAzure(t, srv.URL, true, true)
	c := discoverCloud("Hyper-V")
	want := CloudInstance{Provider: "azure", Account: "6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90", Region: "eastus", Zone: "eastus-2",
		ID: "aaaaaaaa-0000-4000-8000-000000000001", Type: "Standard_B2s"}
	if c == nil || *c != want {
		t.Fatalf("got %+v, want %+v", c, want)
	}
	if discoverCloud("Hyper-V") == nil || asked != 1 {
		t.Fatalf("an answer is kept: asked %d times", asked)
	}
}

// Where the firmware cannot be read, a Hyper-V guest asks once; with no
// answer it must not ask on every report.
func TestAHyperVGuestThatIsNotAzureAsksOnce(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	stubAzure(t, srv.URL, false, false)
	for range 3 {
		if c := discoverCloud("Hyper-V"); c != nil {
			t.Fatalf("got %+v", c)
		}
	}
	if asked != 1 {
		t.Fatalf("asked %d times, want once", asked)
	}
	// And where it can be read and says Azure, it does ask.
	asked = 0
	good := azureMetadata(t, &asked)
	stubAzure(t, good.URL, false, false)
	if discoverCloud("Hyper-V") == nil {
		t.Fatal("unknown firmware and an answer must be believed")
	}
}

func TestCloudInstanceValid(t *testing.T) {
	bad := []CloudInstance{
		{Provider: "aws", Account: "12345", Region: "us-east-1", ID: "i-0a41c27b9e3f1d27d"},
		{Provider: "aws", Account: "412788931204", Region: "us-east-1", ID: "i-<script>"},
		{Provider: "aws", Account: "412788931204", Region: "US EAST", ID: "i-0a41c27b9e3f1d27d"},
		{Provider: "azure", Account: "x", Region: "eastus", ID: "1"},
		{Provider: "gcp", Account: "P", Region: "asia-southeast2", ID: "45"},
	}
	for _, c := range bad {
		if c.valid() {
			t.Errorf("%+v passed", c)
		}
	}
}
