package apk

import (
	"bytes"
	"errors"
	"testing"

	"github.com/gsoultan/argus/internal/apk/apktest"
)

func TestReadsAV2SignedPackage(t *testing.T) {
	der := apktest.Cert(t)
	z := apktest.WithV2(t, apktest.Zip(t, map[string][]byte{
		"AndroidManifest.xml": apktest.Manifest("com.impack.survey", 240, "2.4.0", "Field Survey"),
		"classes.dex":         []byte("dex\n035"),
	}), der, apktest.IDV2)
	info, err := Read(bytes.NewReader(z), int64(len(z)))
	if err != nil {
		t.Fatal(err)
	}
	want := Info{Package: "com.impack.survey", VersionCode: 240, VersionName: "2.4.0", Label: "Field Survey",
		MinSDK: 29, Signer: apktest.SHA256(der), Scheme: "v2"}
	if info != want {
		t.Errorf("got %+v\nwant %+v", info, want)
	}
	// A v3 block wins, and says so.
	z3 := apktest.WithV2(t, apktest.Zip(t, map[string][]byte{"AndroidManifest.xml": apktest.Manifest("com.impack.survey", 1, "1", "")}), der, apktest.IDV3)
	if info, err := Read(bytes.NewReader(z3), int64(len(z3))); err != nil || info.Scheme != "v3" || info.Signer != apktest.SHA256(der) {
		t.Errorf("v3: %+v %v", info, err)
	}
}

func TestReadsAV1SignedPackage(t *testing.T) {
	der := apktest.Cert(t)
	z := apktest.Zip(t, map[string][]byte{
		"AndroidManifest.xml": apktest.Manifest("com.impack.portal", 4120, "4.12.0", ""),
		"META-INF/CERT.RSA":   apktest.PKCS7(t, der),
	})
	info, err := Read(bytes.NewReader(z), int64(len(z)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Scheme != "v1" || info.Signer != apktest.SHA256(der) || info.VersionCode != 4120 {
		t.Errorf("%+v", info)
	}
}

func TestRefusesWhatIsNotASignedPackage(t *testing.T) {
	unsigned := apktest.Zip(t, map[string][]byte{"AndroidManifest.xml": apktest.Manifest("com.impack.survey", 1, "1", "")})
	if _, err := Read(bytes.NewReader(unsigned), int64(len(unsigned))); !errors.Is(err, ErrUnsigned) {
		t.Errorf("unsigned: %v", err)
	}
	noManifest := apktest.Zip(t, map[string][]byte{"classes.dex": []byte("x")})
	if _, err := Read(bytes.NewReader(noManifest), int64(len(noManifest))); !errors.Is(err, ErrNotAPK) {
		t.Errorf("no manifest: %v", err)
	}
	if _, err := Read(bytes.NewReader([]byte("MZ not a zip")), 12); !errors.Is(err, ErrNotAPK) {
		t.Errorf("not a zip: %v", err)
	}
	// A manifest naming something that is not an application id.
	bad := apktest.WithV2(t, apktest.Zip(t, map[string][]byte{"AndroidManifest.xml": apktest.Manifest("x; rm -rf /", 1, "1", "")}), apktest.Cert(t), apktest.IDV2)
	if _, err := Read(bytes.NewReader(bad), int64(len(bad))); err == nil {
		t.Error("a bad package name was accepted")
	}
	// Truncated or corrupt manifests fail cleanly.
	m := apktest.Manifest("com.impack.survey", 1, "1", "")
	for _, cut := range []int{4, 12, 40, len(m) - 7} {
		z := apktest.WithV2(t, apktest.Zip(t, map[string][]byte{"AndroidManifest.xml": m[:cut]}), apktest.Cert(t), apktest.IDV2)
		if _, err := Read(bytes.NewReader(z), int64(len(z))); err == nil {
			t.Errorf("a manifest cut at %d was accepted", cut)
		}
	}
}

func FuzzManifest(f *testing.F) {
	f.Add(apktest.Manifest("com.impack.survey", 240, "2.4.0", "Field Survey"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = parseManifest(b)
	})
}
