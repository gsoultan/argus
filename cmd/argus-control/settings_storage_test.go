package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/storage"
)

// The console's storage card becomes the storage block, and a secret left
// blank on the form is kept -- but only for the provider it belongs to.
func TestStorageSettingsBecomeTheStorageBlock(t *testing.T) {
	prevS3 := &storage.Config{Endpoint: "s3.amazonaws.com", AccessKey: "AKIA", SecretKey: "${secret:s3}"}

	c, err := storageConfigFrom(control.StorageSettings{Enabled: true, Provider: "s3",
		Endpoint: " abc.r2.cloudflarestorage.com ", Bucket: "rec", AccessKey: "k"}, prevS3)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != "" || c.Endpoint != "abc.r2.cloudflarestorage.com" || c.SecretKey != "${secret:s3}" {
		t.Errorf("S3: %+v; want the provider written as nothing and the kept secret", c)
	}

	c, err = storageConfigFrom(control.StorageSettings{Enabled: true, Provider: "azure",
		Bucket: "rec", AccessKey: "acct"}, prevS3)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != storage.ProviderAzure || c.SecretKey != "" {
		t.Errorf("Azure: %+v; an S3 secret key must not become an Azure account key", c)
	}
	if err := validateStorage(c); err == nil {
		t.Error("Azure without an account key was accepted")
	}

	if _, err := storageConfigFrom(control.StorageSettings{Enabled: true, Provider: "gcs"}, nil); err == nil {
		t.Error("an unknown provider was accepted")
	}
}

func TestValidateStorage(t *testing.T) {
	key := "a2V5" // base64 of "key"
	for _, c := range []struct {
		cfg storage.Config
		err string
	}{
		{storage.Config{Endpoint: "s3.amazonaws.com", Bucket: "b", AccessKey: "a", SecretKey: "s"}, ""},
		{storage.Config{Bucket: "b", AccessKey: "a", SecretKey: "s"}, "endpoint"},
		{storage.Config{Provider: "azure", Bucket: "rec", AccessKey: "acct", SecretKey: key}, ""},
		{storage.Config{Provider: "azure", AccessKey: "acct", SecretKey: key}, "container"},
		{storage.Config{Provider: "azure", Bucket: "rec", AccessKey: "acct", SecretKey: "not a key!"}, "not a key"},
		{storage.Config{Provider: "azure", Bucket: "Rec", AccessKey: "acct", SecretKey: key}, "container"},
	} {
		err := validateStorage(&c.cfg)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%+v: %v, want %q", c.cfg, err, c.err)
		}
	}
}

// An archive class has to be one the configured provider has: Azure calls
// them access tiers, and neither S3's names nor Azure's mean anything to the
// other.
func TestArchiveClassFollowsTheProvider(t *testing.T) {
	az := &storage.Config{Provider: storage.ProviderAzure}
	s3 := &storage.Config{}
	for _, c := range []struct {
		st    *storage.Config
		class string
		ok    bool
	}{
		{az, "", true}, {az, "Cool", true}, {az, "Cold", true},
		{az, "GLACIER_IR", false}, {az, "Archive", false},
		{s3, "GLACIER_IR", true}, {s3, "Cool", false}, {nil, "STANDARD_IA", true},
	} {
		if err := checkArchiveClass(c.st, c.class); (err == nil) != c.ok {
			t.Errorf("%+v %q: %v", c.st, c.class, err)
		}
	}
}

// Testing storage from the console proves the whole round trip against the
// service. Runs against Azurite; see internal/storage/azure_test.go.
func TestTestStorageWritesAndReadsBack(t *testing.T) {
	endpoint := os.Getenv("ARGUS_TEST_AZURITE")
	if endpoint == "" {
		t.Skip("ARGUS_TEST_AZURITE is not set")
	}
	b := &settingsBackend{cfg: config{}}
	in := control.StorageSettings{Enabled: true, Provider: "azure", Endpoint: endpoint,
		Bucket: "argus-console-check", AccessKey: "devstoreaccount1",
		SecretKey: ptr("Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==")}
	if err := b.TestStorage(context.Background(), in); err != nil {
		t.Fatalf("a working account: %v", err)
	}
	in.SecretKey = ptr("d3Jvbmc=")
	if err := b.TestStorage(context.Background(), in); err == nil || !strings.Contains(err.Error(), "could not reach") {
		t.Errorf("a wrong key: %v, want a refusal naming the step", err)
	}
}
