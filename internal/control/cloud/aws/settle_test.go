package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
What AWS is still doing must be read again soon. Root cause: only a state
named as settling had its region read again, and a disk being detached is
still "in-use" while a target group waiting on a new target's first health
check is only "degraded", so both showed where they were for up to five
minutes.
*/

func TestADiskBeingDetachedOrAttachedIsSettling(t *testing.T) {
	vol := func(st types.VolumeAttachmentState) cloud.Resource {
		return Volume(types.Volume{VolumeId: awssdk.String("vol-1"), State: types.VolumeStateInUse, Size: awssdk.Int32(20),
			Attachments: []types.VolumeAttachment{{InstanceId: awssdk.String("i-1"), Device: awssdk.String("/dev/sdf"), State: st}}}, "eu-west-1")
	}
	for _, st := range []types.VolumeAttachmentState{types.VolumeAttachmentStateDetaching, types.VolumeAttachmentStateAttaching} {
		if r := vol(st); !r.Settling() {
			t.Fatalf("a disk that is %s is not read again soon: %+v", st, r)
		}
	}
	if r := vol(types.VolumeAttachmentStateAttached); r.Settling() {
		t.Fatalf("an attached disk is settled: %+v", r)
	}
}

func TestATargetGroupWaitingOnATargetIsSettling(t *testing.T) {
	tg := func(states ...elbtypes.TargetHealthStateEnum) cloud.Resource {
		var hs []elbtypes.TargetHealthDescription
		for _, s := range states {
			hs = append(hs, elbtypes.TargetHealthDescription{Target: &elbtypes.TargetDescription{Id: awssdk.String("i-1")},
				TargetHealth: &elbtypes.TargetHealth{State: s}})
		}
		return TargetGroup(elbtypes.TargetGroup{TargetGroupArn: awssdk.String("arn:tg"), TargetType: elbtypes.TargetTypeEnumInstance,
			LoadBalancerArns: []string{"arn:lb"}}, "eu-west-1", nil, hs, nil)
	}
	if r := tg(elbtypes.TargetHealthStateEnumHealthy, elbtypes.TargetHealthStateEnumInitial); !r.Settling() {
		t.Fatalf("a new target's first check is not followed: %+v", r)
	}
	if r := tg(elbtypes.TargetHealthStateEnumDraining); !r.Settling() {
		t.Fatalf("a draining target is not followed: %+v", r)
	}
	// Unhealthy is where it is, not on its way somewhere.
	if r := tg(elbtypes.TargetHealthStateEnumHealthy, elbtypes.TargetHealthStateEnumUnhealthy); r.Settling() {
		t.Fatalf("an unhealthy target is not settling: %+v", r)
	}
}

/*
People and keys must be in an account's first read. Root cause: the read
asked AWS to make the credential report and fetched it in the same breath;
AWS takes a few seconds, so the first read of every account (and the first
after a report expired) had none, and Risks & waste could not check users
or the root account until the read five minutes later.
*/

type fakeReport struct {
	states []iamtypes.ReportStateType
	asked  int
	getErr error
}

func (f *fakeReport) GenerateCredentialReport(context.Context, *iam.GenerateCredentialReportInput, ...func(*iam.Options)) (*iam.GenerateCredentialReportOutput, error) {
	s := f.states[min(f.asked, len(f.states)-1)]
	f.asked++
	return &iam.GenerateCredentialReportOutput{State: s}, nil
}

func (f *fakeReport) GetCredentialReport(context.Context, *iam.GetCredentialReportInput, ...func(*iam.Options)) (*iam.GetCredentialReportOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &iam.GetCredentialReportOutput{Content: []byte("user,arn,mfa_active\n<root_account>,arn:aws:iam::111122223333:root,true\n")}, nil
}

func TestCredentialReportIsWaitedFor(t *testing.T) {
	oldWait, oldPoll := reportWait, reportPoll
	reportWait, reportPoll = time.Second, time.Millisecond
	t.Cleanup(func() { reportWait, reportPoll = oldWait, oldPoll })

	f := &fakeReport{states: []iamtypes.ReportStateType{iamtypes.ReportStateTypeStarted, iamtypes.ReportStateTypeInprogress, iamtypes.ReportStateTypeComplete}}
	rs, err := credentialReport(context.Background(), f, "111122223333")
	if err != nil || len(rs) != 1 || rs[0].Kind != cloud.KindAccount {
		t.Fatalf("a report AWS finished making during the read: %v %+v", err, rs)
	}
	if f.asked != 3 {
		t.Fatalf("asked %d times", f.asked)
	}

	// One AWS never finishes is given up on, and says so plainly.
	reportWait = 5 * time.Millisecond
	never := &fakeReport{states: []iamtypes.ReportStateType{iamtypes.ReportStateTypeInprogress}}
	if _, err := credentialReport(context.Background(), never, "111122223333"); !errors.Is(err, errReportNotReady) {
		t.Fatalf("a report still being made: %v", err)
	}
	// So is one AWS says is not there after all.
	gone := &fakeReport{states: []iamtypes.ReportStateType{iamtypes.ReportStateTypeComplete}, getErr: &iamtypes.CredentialReportNotPresentException{}}
	if _, err := credentialReport(context.Background(), gone, "111122223333"); !errors.Is(err, errReportNotReady) {
		t.Fatalf("a report not present: %v", err)
	}
}
