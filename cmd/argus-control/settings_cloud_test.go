package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gsoultan/argus/internal/control"
)

const (
	testKeyID  = "AKIAIOSFODNN7EXAMPLE"
	testSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func keyID(id, name, key string, secret *string) control.AWSIdentitySettings {
	return control.AWSIdentitySettings{ID: id, Name: name, Source: control.IdentityKey, AccessKeyID: key, SecretAccessKey: secret}
}

func identities(def string, list ...control.AWSIdentitySettings) *control.CloudSettings {
	if list == nil {
		list = []control.AWSIdentitySettings{}
	}
	return &control.CloudSettings{AWS: control.AWSCloudSettings{Default: def, Identities: list}}
}

func keyIdentity(key string, secret *string) *control.CloudSettings {
	return identities("", keyID("main", "Main", key, secret))
}

func refPath(ref string) string { return strings.TrimSuffix(strings.TrimPrefix(ref, "${file:"), "}") }

// A key typed in Settings goes to an owner-only file of its own; the
// configuration holds only a reference to it, and the console is told only
// that it is set.
func TestCloudKeyIsWrittenOwnerOnly(t *testing.T) {
	st := &secretStage{dir: filepath.Join(t.TempDir(), "secrets")}
	secret := testSecret
	c := &config{}
	if err := applyCloud(c, keyIdentity(testKeyID, &secret), st); err != nil {
		t.Fatal(err)
	}
	ref := c.Cloud.AWS.Identities[0].SecretAccessKey
	if strings.Contains(ref, testSecret) || !strings.HasPrefix(ref, "${file:") {
		t.Fatalf("secret_access_key = %q, want a ${file:} reference and never the key", ref)
	}
	path := refPath(ref)
	if !strings.HasPrefix(filepath.Base(path), "aws-main-secret-access-key") {
		t.Errorf("secret file %s, want one named after the identity", path)
	}
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
	resolved.AWS.Identities = []awsIdentityConfig{resolved.AWS.Identities[0]}
	resolved.AWS.Identities[0].SecretAccessKey = testSecret
	shown := cloudSettingsOf(&resolved).AWS.Identities[0]
	if shown.Source != control.IdentityKey || !shown.SecretAccessKeySet || shown.SecretAccessKey != nil {
		t.Errorf("shown = %+v, want source key, secret set and never sent back", shown)
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
	if got := c.Cloud.AWS.Identities[0].SecretAccessKey; got != prev.AWS.SecretAccessKey {
		t.Errorf("secret = %q, want the configured one kept", got)
	}
	c = &config{Cloud: prev}
	err := applyCloud(c, keyIdentity("AKIAI44QH8DHBEXAMPLE", nil), nil)
	if err == nil || !strings.Contains(err.Error(), "secret access key") {
		t.Errorf("new key id without its secret: err = %v, want a refusal", err)
	}
}

// One identity's secret is never another's, even when it has the same key
// id: the secret is kept by identity id.
func TestCloudSecretIsKeptByIdentity(t *testing.T) {
	prev := &cloudConfig{AWS: awsCloudConfig{Identities: []awsIdentityConfig{
		{ID: "main", Name: "Main", AccessKeyID: testKeyID, SecretAccessKey: "${file:/x/a}"}}}}
	err := applyCloud(&config{Cloud: prev}, identities("", keyID("main", "Main", testKeyID, nil),
		keyID("", "Second", testKeyID, nil)), nil)
	if err == nil || !strings.Contains(err.Error(), "Second") {
		t.Errorf("err = %v, want the new identity refused for want of its own secret", err)
	}
}

func TestCloudRefusesBadIdentities(t *testing.T) {
	secret := testSecret
	short := "short"
	host := func(name string) control.AWSIdentitySettings {
		return control.AWSIdentitySettings{Name: name, Source: control.IdentityDefault}
	}
	for name, in := range map[string]*control.CloudSettings{
		"temporary":  keyIdentity("ASIAIOSFODNN7EXAMPLE", &secret),
		"malformed":  keyIdentity("not-a-key", &secret),
		"no id":      keyIdentity("", &secret),
		"short":      keyIdentity(testKeyID, &short),
		"no secret":  keyIdentity(testKeyID, nil),
		"bad region": identities("", control.AWSIdentitySettings{Name: "A", HomeRegion: "mars"}),
		"bad source": identities("", control.AWSIdentitySettings{Name: "A", Source: "ftp"}),
		"empty file": identities("", control.AWSIdentitySettings{Name: "A", Source: control.IdentityFile}),
		"relative":   identities("", control.AWSIdentitySettings{Name: "A", Source: control.IdentityFile, CredentialsFile: "aws"}),
		"no name":    identities("", host("")),
		"two hosts":  identities("", host("A"), host("B")),
		"same name":  identities("", keyID("", "A", testKeyID, &secret), host("a")),
		"bad id":     identities("", control.AWSIdentitySettings{ID: "../x", Name: "A"}),
		"same id":    identities("", control.AWSIdentitySettings{ID: "x", Name: "A"}, keyID("x", "B", testKeyID, &secret)),
		"no default": identities("nope", host("A")),
		"none":       identities(""),
		"bad org":    identities("", control.AWSIdentitySettings{Name: "A", Organizations: []string{"Org One"}}),
		"long name":  identities("", host(strings.Repeat("n", 65))),
	} {
		if err := applyCloud(&config{}, in, nil); err == nil {
			t.Errorf("%s: accepted, want a refusal", name)
		}
	}
	var many []control.AWSIdentitySettings
	for n := range control.MaxCloudIdentities + 1 {
		many = append(many, control.AWSIdentitySettings{Name: "p" + string(rune('a'+n)), Source: control.IdentityFile, Profile: "p"})
	}
	if err := applyCloud(&config{}, identities("", many...), nil); err == nil {
		t.Error("more than the limit accepted")
	}
}

// A credentials file others can read is refused at save, not at the next
// restart.
func TestCloudRefusesAReadableCredentialsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "aws-credentials")
	if err := os.WriteFile(f, []byte("[default]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := identities("", control.AWSIdentitySettings{Name: "Files", Source: control.IdentityFile, CredentialsFile: f})
	if err := applyCloud(&config{}, in, nil); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("err = %v, want the mode refused", err)
	}
	if err := os.Chmod(f, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &config{}
	if err := applyCloud(c, in, nil); err != nil || c.Cloud.AWS.Identities[0].CredentialsFile != f {
		t.Errorf("owner-only file: err = %v, cloud = %+v", err, c.Cloud)
	}
}

// The host's own identity alone, named Main, is no cloud block at all; a
// console older than several identities keeps what is there.
func TestCloudHostAloneClearsAndNilKeeps(t *testing.T) {
	prev := &cloudConfig{AWS: awsCloudConfig{AccessKeyID: testKeyID, SecretAccessKey: "${file:/x}"}}
	c := &config{Cloud: prev}
	if err := applyCloud(c, nil, nil); err != nil || c.Cloud != prev {
		t.Errorf("nil: err = %v, cloud = %+v, want kept", err, c.Cloud)
	}
	old := &control.CloudSettings{} // the one-identity console sends no list
	if err := applyCloud(c, old, nil); err != nil || c.Cloud != prev {
		t.Errorf("no list: err = %v, cloud = %+v, want kept", err, c.Cloud)
	}
	in := identities("main", control.AWSIdentitySettings{ID: "main", Name: "Main", Source: control.IdentityDefault})
	if err := applyCloud(c, in, nil); err != nil || c.Cloud != nil {
		t.Errorf("host alone: err = %v, cloud = %+v, want removed", err, c.Cloud)
	}
	in.AWS.Identities[0].HomeRegion = "eu-west-1"
	if err := applyCloud(c, in, nil); err != nil || c.Cloud == nil || c.Cloud.AWS.Identities[0].HomeRegion != "eu-west-1" {
		t.Errorf("host with a region: err = %v, cloud = %+v", err, c.Cloud)
	}
}

// A config from before several identities is one identity, main, the
// default; no config at all is the host's own identity under that id. Both
// still start.
func TestCloudOldConfigIsMain(t *testing.T) {
	var c config
	if err := yaml.Unmarshal([]byte("cloud:\n  aws:\n    access_key_id: "+testKeyID+
		"\n    secret_access_key: ${file:/s/aws-secret-access-key}\n    home_region: eu-west-1\n"), &c); err != nil {
		t.Fatal(err)
	}
	if err := validateCloud(c.Cloud); err != nil {
		t.Fatal(err)
	}
	list, def := c.Cloud.awsIdentities()
	if def != "main" || len(list) != 1 || list[0].ID != "main" || list[0].AccessKeyID != testKeyID ||
		list[0].HomeRegion != "eu-west-1" || list[0].SecretAccessKey == "" || len(list[0].Organizations) != 0 {
		t.Errorf("old config read as %+v default %q", list, def)
	}
	ids := c.Cloud.cloudIdentities()
	if len(ids) != 1 || !ids[0].Default || ids[0].ID != control.DefaultCloudIdentity {
		t.Errorf("registered as %+v", ids)
	}
	var none *cloudConfig
	if list, def := none.awsIdentities(); def != "main" || len(list) != 1 || list[0].source() != control.IdentityDefault {
		t.Errorf("no block read as %+v default %q", list, def)
	}
}

// Several identities, each with its own secret file, organizations and the
// default; new ones get an id from their name.
func TestCloudSeveralIdentities(t *testing.T) {
	st := &secretStage{dir: filepath.Join(t.TempDir(), "secrets")}
	s1, s2 := testSecret, strings.Replace(testSecret, "w", "v", 1)
	c := &config{}
	err := applyCloud(c, identities("Northwind's IAM user",
		keyID("", "Main", testKeyID, &s1),
		keyID("", "Northwind's IAM user", "AKIAI44QH8DHBEXAMPLE", &s2),
		control.AWSIdentitySettings{Name: "GovCloud", Source: control.IdentityFile, Profile: "gov", HomeRegion: "us-gov-west-1",
			Organizations: []string{"default", "t0abc", "t0abc"}},
	), st)
	if err != nil {
		t.Fatal(err)
	}
	got := c.Cloud.AWS
	if len(got.Identities) != 3 || got.Identities[0].ID != "main" || got.Identities[1].ID != "northwinds-iam-user" ||
		got.Identities[2].ID != "govcloud" || got.Default != "northwinds-iam-user" {
		t.Fatalf("identities = %+v default %q", got.Identities, got.Default)
	}
	if o := got.Identities[2].Organizations; len(o) != 2 {
		t.Errorf("organizations = %v, want each once", o)
	}
	if refPath(got.Identities[0].SecretAccessKey) == refPath(got.Identities[1].SecretAccessKey) {
		t.Error("two identities share a secret file")
	}
	if err := validateCloud(c.Cloud); err != nil {
		t.Errorf("a saved list does not validate: %v", err)
	}
}

// Removing an identity, or giving it a new key, deletes the secret file it
// had -- but only once the save worked, and only in the secrets directory.
func TestCloudRemovedSecretIsDeleted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "aws-gone-secret-access-key.1")
	outside := filepath.Join(t.TempDir(), "mine")
	for _, f := range []string{old, outside} {
		if err := os.WriteFile(f, []byte(testSecret), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prev := &cloudConfig{AWS: awsCloudConfig{Identities: []awsIdentityConfig{
		{ID: "main", Name: "Main"},
		{ID: "gone", Name: "Gone", AccessKeyID: testKeyID, SecretAccessKey: "${file:" + old + "}"},
		{ID: "hand", Name: "Hand", AccessKeyID: testKeyID, SecretAccessKey: "${file:" + outside + "}"},
	}}}
	st := &secretStage{dir: dir}
	c := &config{Cloud: prev}
	if err := applyCloud(c, identities("main", control.AWSIdentitySettings{ID: "main", Name: "Main"}), st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("deleted before the save worked: %v", err)
	}
	st.discard() // a refused save keeps it
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("a refused save deleted it: %v", err)
	}
	st.prune()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("removed identity's secret still there: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file outside the secrets directory was deleted: %v", err)
	}
}

func TestValidateCloudRefusesHandEdits(t *testing.T) {
	for name, c := range map[string]*cloudConfig{
		"bad id":     {AWS: awsCloudConfig{Identities: []awsIdentityConfig{{ID: "A B"}}}},
		"same id":    {AWS: awsCloudConfig{Identities: []awsIdentityConfig{{ID: "a", Profile: "x"}, {ID: "a", Profile: "y"}}}},
		"two hosts":  {AWS: awsCloudConfig{Identities: []awsIdentityConfig{{ID: "a"}, {ID: "b"}}}},
		"no default": {AWS: awsCloudConfig{Default: "c", Identities: []awsIdentityConfig{{ID: "a"}}}},
	} {
		if err := validateCloud(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
