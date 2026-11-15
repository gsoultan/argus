package aws

import (
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/gsoultan/argus/internal/control/cloud"
)

func TestParseCredentialReport(t *testing.T) {
	csv := "user,arn,user_creation_time,password_enabled,password_last_used,password_last_changed,password_next_rotation,mfa_active,access_key_1_active,access_key_1_last_rotated,access_key_1_last_used_date,access_key_1_last_used_region,access_key_1_last_used_service,access_key_2_active,access_key_2_last_rotated,access_key_2_last_used_date,access_key_2_last_used_region,access_key_2_last_used_service,cert_1_active,cert_1_last_rotated,cert_2_active,cert_2_last_rotated\n" +
		"<root_account>,arn:aws:iam::123:root,2020-01-01T00:00:00+00:00,not_supported,2026-11-01T00:00:00+00:00,not_supported,not_supported,false,true,2021-01-01T00:00:00+00:00,N/A,N/A,N/A,false,N/A,N/A,N/A,N/A,false,N/A,false,N/A\n" +
		"ana,arn:aws:iam::123:user/ana,2024-01-01T00:00:00+00:00,true,2026-11-10T00:00:00+00:00,N/A,N/A,false,true,2025-01-01T00:00:00+00:00,2026-06-01T10:00:00+00:00,us-east-1,s3,false,N/A,N/A,N/A,N/A,false,N/A,false,N/A\n"
	rs, err := ParseCredentialReport([]byte(csv), "123")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d resources", len(rs))
	}
	root := rs[0]
	if root.Kind != cloud.KindAccount || root.Details["rootMfa"] != "no" || root.Details["rootKeys"] != "1" {
		t.Fatalf("root = %+v", root)
	}
	ana := rs[1]
	if ana.Kind != cloud.KindUser || ana.Name != "ana" || ana.Details["password"] != "yes" || ana.Details["mfa"] != "no" ||
		ana.Details["key1Active"] != "yes" || ana.Details["key1LastUsed"] != "2026-06-01T10:00:00Z" || ana.Details["key2Active"] != "" {
		t.Fatalf("ana = %+v", ana.Details)
	}
}

func TestBlockState(t *testing.T) {
	y, n := awssdk.Bool(true), awssdk.Bool(false)
	for want, flags := range map[string][]*bool{"all": {y, y, y, y}, "none": {n, nil, n, n}, "partial": {y, n, y, y}} {
		if got := BlockState(flags...); got != want {
			t.Errorf("%v: got %s, want %s", flags, got, want)
		}
	}
}

func TestMarkGroupUse(t *testing.T) {
	fws := []cloud.Resource{{Kind: cloud.KindFirewall, ID: "sg-a"}, {Kind: cloud.KindFirewall, ID: "sg-b", Details: map[string]string{}}}
	MarkGroupUse(fws, map[string][]string{"sg-a": {"instance", "instance", "AWS service"}})
	if fws[0].Details["interfaces"] != "3" || fws[0].Details["interfaceKinds"] != "instance, AWS service" {
		t.Fatalf("sg-a = %+v", fws[0].Details)
	}
	if fws[1].Details["interfaces"] != "0" {
		t.Fatal("a group read and used by nothing says 0, so it can be called unused")
	}
}
