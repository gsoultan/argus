package tunnel

import (
	"strings"
	"testing"
	"time"
)

func TestRecoveryOrdersVerifyOnlyUnderTheirKey(t *testing.T) {
	pub, priv, _ := NewRecoveryKey()
	otherPub, _, _ := NewRecoveryKey()
	grant, _, _ := NewRecoveryKey()
	tok, err := SignRecovery(priv, RecoveryOrder{Kind: RecoveryRekey, Key: EncodeGrantKey(grant), IssuedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	o, err := VerifyRecovery(pub, tok)
	if err != nil || o.Kind != RecoveryRekey || o.Key != EncodeGrantKey(grant) {
		t.Fatalf("verify: %+v %v", o, err)
	}
	if _, err := VerifyRecovery(otherPub, tok); err == nil {
		t.Error("verified under another key")
	}
	body, sig, _ := strings.Cut(tok, ".")
	if _, err := VerifyRecovery(pub, body+"x."+sig); err == nil {
		t.Error("a changed body verified")
	}
	if _, err := SignRecovery(priv, RecoveryOrder{Kind: "anything", Key: EncodeGrantKey(grant)}); err == nil {
		t.Error("signed an unknown kind")
	}
}

func TestRecoveryKeyFileRoundTrips(t *testing.T) {
	pub, priv, _ := NewRecoveryKey()
	file := EncodeRecoveryPrivate(priv)
	if !strings.Contains(file, RecoveryFingerprint(pub)) {
		t.Errorf("the file does not name its fingerprint:\n%s", file)
	}
	back, err := DecodeRecoveryPrivate(file)
	if err != nil || !back.Equal(priv) {
		t.Fatalf("decode: %v", err)
	}
	for _, bad := range []string{"", "hello\nAAAA", recoveryFileHeader + "\nnot-base64!"} {
		if _, err := DecodeRecoveryPrivate(bad); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
}
