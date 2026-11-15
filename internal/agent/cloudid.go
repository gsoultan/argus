package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

/*
Which cloud instance this machine is, read from the cloud's own metadata
service, so the console can show a server and its instance as one thing
instead of guessing from addresses.

Asked only when the firmware already names the cloud (the hypervisor read
from DMI is "Amazon EC2" or "Google Compute Engine"), so a laptop or a
server in a rack never waits on a link-local address that is not there. The
metadata service is on the machine's own link (169.254.169.254), never
reached through a proxy, and answers in milliseconds; two seconds is the
whole budget.

Read-only, and nothing a local user could not read: the instance identity
document on AWS (IMDSv2, a session token first, as every hardened instance
requires) and four plain values on Google Cloud. No credentials, no user
data, no tags. A successful answer is kept for the life of the process: an
instance's id, account and region do not change while it runs, and a resize
needs a stop, which restarts the agent.
*/

// CloudInstance is the instance this machine runs as.
type CloudInstance struct {
	// Provider is "aws" or "gcp".
	Provider string `json:"provider"`
	// Account is the AWS account number, or the Google Cloud project id.
	Account string `json:"account"`
	Region  string `json:"region"`
	Zone    string `json:"zone,omitempty"`
	// ID is the instance id: i-0123… on AWS, a number on Google Cloud.
	ID   string `json:"id"`
	Type string `json:"type,omitempty"`
}

const cloudMetadataBudget = 2 * time.Second

var (
	cloudMu   sync.Mutex
	cloudCached *CloudInstance
)

// metadataBase is where the services answer; a test points it elsewhere.
var (
	awsMetadataBase = "http://169.254.169.254"
	gcpMetadataBase = "http://169.254.169.254"
)

// discoverCloud returns the instance for a machine whose firmware names a
// cloud, or nil. A failure is not kept, so the next report asks again.
func discoverCloud(hypervisor string) *CloudInstance {
	var read func(context.Context, *http.Client) *CloudInstance
	switch hypervisor {
	case "Amazon EC2":
		read = readAWSInstance
	case "Google Compute Engine":
		read = readGCPInstance
	default:
		return nil
	}
	cloudMu.Lock()
	defer cloudMu.Unlock()
	if cloudCached != nil {
		c := *cloudCached
		return &c
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudMetadataBudget)
	defer cancel()
	c := read(ctx, metadataClient())
	if c == nil || !c.valid() {
		return nil
	}
	cloudCached = c
	out := *c
	return &out
}

// metadataClient never uses a proxy: the address is on this machine's link,
// and a proxy that answered for it would be answering for another machine.
func metadataClient() *http.Client {
	return &http.Client{
		Timeout: cloudMetadataBudget,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true,
			ResponseHeaderTimeout: cloudMetadataBudget},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func readAWSInstance(ctx context.Context, hc *http.Client) *CloudInstance {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, awsMetadataBase+"/latest/api/token", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	token, ok := metadataGet(hc, req)
	if !ok || token == "" {
		return nil
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, awsMetadataBase+"/latest/dynamic/instance-identity/document", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-aws-ec2-metadata-token", token)
	body, ok := metadataGet(hc, req)
	if !ok {
		return nil
	}
	var doc struct {
		AccountID        string `json:"accountId"`
		Region           string `json:"region"`
		AvailabilityZone string `json:"availabilityZone"`
		InstanceID       string `json:"instanceId"`
		InstanceType     string `json:"instanceType"`
	}
	if json.Unmarshal([]byte(body), &doc) != nil {
		return nil
	}
	return &CloudInstance{Provider: "aws", Account: doc.AccountID, Region: doc.Region,
		Zone: doc.AvailabilityZone, ID: doc.InstanceID, Type: doc.InstanceType}
}

func readGCPInstance(ctx context.Context, hc *http.Client) *CloudInstance {
	get := func(path string) string {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, gcpMetadataBase+"/computeMetadata/v1/"+path, nil)
		if err != nil {
			return ""
		}
		req.Header.Set("Metadata-Flavor", "Google")
		v, _ := metadataGet(hc, req)
		return strings.TrimSpace(v)
	}
	id := get("instance/id")
	if id == "" {
		return nil
	}
	// zone and machine-type come as projects/<number>/zones/<zone> and
	// projects/<number>/machineTypes/<type>; the last part is the name.
	zone := lastPart(get("instance/zone"))
	region := zone
	if i := strings.LastIndex(zone, "-"); i > 0 {
		region = zone[:i]
	}
	return &CloudInstance{Provider: "gcp", Account: get("project/project-id"), Region: region,
		Zone: zone, ID: id, Type: lastPart(get("instance/machine-type"))}
}

// metadataGet reads a small answer; anything but 200 is no answer.
func metadataGet(hc *http.Client, req *http.Request) (string, bool) {
	resp, err := hc.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func lastPart(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

var (
	awsInstanceRE = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
	awsAccountRE  = regexp.MustCompile(`^[0-9]{12}$`)
	gcpInstanceRE = regexp.MustCompile(`^[0-9]{1,20}$`)
	gcpProjectRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	cloudPlaceRE  = regexp.MustCompile(`^[a-z0-9-]{2,40}$`)
	cloudTypeRE   = regexp.MustCompile(`^[a-z0-9.-]{1,60}$`)
)

// valid is what the control plane will also check: an answer that does not
// look like an instance is no answer.
func (c *CloudInstance) valid() bool {
	if !cloudPlaceRE.MatchString(c.Region) || (c.Zone != "" && !cloudPlaceRE.MatchString(c.Zone)) ||
		(c.Type != "" && !cloudTypeRE.MatchString(c.Type)) {
		return false
	}
	switch c.Provider {
	case "aws":
		return awsInstanceRE.MatchString(c.ID) && awsAccountRE.MatchString(c.Account)
	case "gcp":
		return gcpInstanceRE.MatchString(c.ID) && gcpProjectRE.MatchString(c.Account)
	}
	return false
}
