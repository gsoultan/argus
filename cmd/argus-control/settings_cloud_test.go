package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control"
)

const (
	testKeyID  = "AKIAIOSFODNN7EXAMPLE"
	testSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func keyIdentity(id string, secret *string) *control.CloudSettings {
	return &control.CloudSettings{AWS: control.AWSIdentitySettings{
		Source: control.IdentityKey, AccessKeyID: id, SecretAccessKey: secret}}
}

// A key typed in Settings goes to an owner-only file; the configuration
// holds only a reference to it, and the console is told only that it is set.
func TestCloudKeyIsWrittenOwnerOnly(t *testing.T) {
	st := &secretStage{dir: filepath.Join(t.TempDir(), "secrets")}
	secret := testSecret
	c := &config{}
	if err := applyCloud(c, keyIdentity(testKeyID, &secret), st); err != nil {
		t.Fatal(err)
	}
	ref := c.Cloud.AWS.SecretAccessKey
	if strings.Contains(ref, testSecret) || !strings.HasPrefix(ref, "${file:") {
		t.Fatalf("secret_access_key = %q, want a ${file:} reference and never the key", ref)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(ref, "${file:"), "}")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secret file mode = %04o, want 0600", perm)
	}
	if body, _ := os.ReadFile(path); strings.TrimSpace(string(body)) != testSecret {
		t.Errorf("secret file holds %q", body)
	}

	resolved := *c.Cloud
	resolved.AWS.SecretAccessKey = testSecret
	shown := cloudSettingsOf(&resolved)
	if shown.AWS.Source != control.IdentityKey || !shown.AWS.SecretAccessKeySet || shown.AWS.SecretAccessKey != nil {
		t.Errorf("shown = %+v, want source key, secret set and never sent back", shown.AWS)
	}
}

// Leaving the secret out keeps it, but only for the key id it belongs to:
// a new key id with the old secret would be a key that does not exist.
func TestCloudSecretKeptOnlyForItsKey(t *testing.T) {
	prev := &cloudConfig{AWS: awsCloudConfig{AccessKeyID: testKeyID, SecretAccessKey: "${file:/x/aws-secret-access-key.1}"}}
	c := &config{Cloud: prev}
	if err := applyCloud(c, keyIdentity(testKeyID, nil), nil); err != nil {
		t.Fatal(err)
	}
	if c.Cloud.AWS.SecretAccessKey != prev.AWS.SecretAccessKey {
		t.Errorf("secret = %q, want the configured one kept", c.Cloud.AWS.SecretAccessKey)
	}
	c = &config{Cloud: prev}
	err := applyCloud(c, keyIdentity("AKIAI44QH8DHBEXAMPLE", nil), nil)
	if err == nil || !strings.Contains(err.Error(), "secret access key") {
		t.Errorf("new key id without its secret: err = %v, want a refusal", err)
	}
}

func TestCloudRefusesBadKeys(t *testing.T) {
	secret := testSecret
	short := "short"
	for name, in := range map[string]*control.CloudSettings{
		"temporary":  keyIdentity("ASIAIOSFODNN7EXAMPLE", &secret),
		"malformed":  keyIdentity("not-a-key", &secret),
		"no id":      keyIdentity("", &secret),
		"short":      keyIdentity(testKeyID, &short),
		"no secret":  keyIdentity(testKeyID, nil),
		"bad region": {AWS: control.AWSIdentitySettings{Source: control.IdentityDefault, HomeRegion: "mars"}},
		"bad source": {AWS: control.AWSIdentitySettings{Source: "ftp"}},
		"empty file": {AWS: control.AWSIdentitySettings{Source: control.IdentityFile}},
		"relative":   {AWS: control.AWSIdentitySettings{Source: control.IdentityFile, CredentialsFile: "aws"}},
	} {
		if err := applyCloud(&config{}, in, nil); err == nil {
			t.Errorf("%s: accepted, want a refusal", name)
		}
	}
}

// A credentials file others can read is refused at save, not at the next
// restart.
func TestCloudRefusesAReadableCredentialsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "aws-credentials")
	if err := os.WriteFile(f, []byte("[default]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := &control.CloudSettings{AWS: control.AWSIdentitySettings{Source: control.IdentityFile, CredentialsFile: f}}
	if err := applyCloud(&config{}, in, nil); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("err = %v, want the mode refused", err)
	}
	if err := os.Chmod(f, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &config{}
	if err := applyCloud(c, in, nil); err != nil || c.Cloud.AWS.CredentialsFile != f {
		t.Errorf("owner-only file: err = %v, cloud = %+v", err, c.Cloud)
	}
}

// Going back to the host's own identity removes the block; a console older
// than Cloud identities keeps it.
func TestCloudDefaultClearsAndNilKeeps(t *testing.T) {
	prev := &cloudConfig{AWS: awsCloudConfig{AccessKeyID: testKeyID, SecretAccessKey: "${file:/x}"}}
	c := &config{Cloud: prev}
	if err := applyCloud(c, nil, nil); err != nil || c.Cloud != prev {
		t.Errorf("nil: err = %v, cloud = %+v, want kept", err, c.Cloud)
	}
	in := &control.CloudSettings{AWS: control.AWSIdentitySettings{Source: control.IdentityDefault}}
	if err := applyCloud(c, in, nil); err != nil || c.Cloud != nil {
		t.Errorf("default: err = %v, cloud = %+v, want removed", err, c.Cloud)
	}
	in.AWS.HomeRegion = "eu-west-1"
	if err := applyCloud(c, in, nil); err != nil || c.Cloud == nil || c.Cloud.AWS.HomeRegion != "eu-west-1" {
		t.Errorf("default with a region: err = %v, cloud = %+v", err, c.Cloud)
	}
}
