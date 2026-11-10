package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control"
	awscloud "github.com/gsoultan/argus/internal/control/cloud/aws"
	"github.com/gsoultan/argus/internal/secrets"
)

// The secret's file under the secrets directory.
const awsSecretName = "aws-secret-access-key"

var (
	// An IAM user's long-lived key. ASIA keys are temporary, and expire.
	reAWSAccessKeyID = regexp.MustCompile(`^AKIA[A-Z0-9]{16}$`)
	reAWSRegion      = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
)

// cloudSettingsOf is the configured identities as the console shows them.
// c is the running configuration, references resolved, so the secret is
// only ever reported as set.
func cloudSettingsOf(c *cloudConfig) *control.CloudSettings {
	out := &control.CloudSettings{AWS: control.AWSIdentitySettings{Source: control.IdentityDefault}}
	if c == nil {
		return out
	}
	a := c.AWS
	out.AWS = control.AWSIdentitySettings{
		Source: control.IdentityDefault, AccessKeyID: a.AccessKeyID, SecretAccessKeySet: a.SecretAccessKey != "",
		Profile: a.Profile, CredentialsFile: a.CredentialsFile, HomeRegion: a.HomeRegion,
	}
	switch {
	case a.AccessKeyID != "":
		out.AWS.Source = control.IdentityKey
	case a.Profile != "" || a.CredentialsFile != "":
		out.AWS.Source = control.IdentityFile
	}
	return out
}

// applyCloud writes in's identities into c. Nil keeps what is configured.
func applyCloud(c *config, in *control.CloudSettings, st *secretStage) error {
	if in == nil {
		return nil
	}
	var prev awsCloudConfig
	if c.Cloud != nil {
		prev = c.Cloud.AWS
	}
	next, err := awsConfigFrom(in.AWS, prev, st)
	if err != nil {
		return control.BadInput(err)
	}
	if next == (awsCloudConfig{}) {
		c.Cloud = nil
		return nil
	}
	c.Cloud = &cloudConfig{AWS: next}
	return nil
}

// awsConfigFrom is in as configuration. prev is what is configured now, as
// written in the file: its secret is the reference, which a form that leaves
// the secret out keeps. With st nil the secret is used as typed, for a test
// that saves nothing.
func awsConfigFrom(in control.AWSIdentitySettings, prev awsCloudConfig, st *secretStage) (awsCloudConfig, error) {
	out := awsCloudConfig{HomeRegion: strings.TrimSpace(in.HomeRegion)}
	if out.HomeRegion != "" && !reAWSRegion.MatchString(out.HomeRegion) {
		return awsCloudConfig{}, fmt.Errorf("%q is not an AWS region, such as us-east-1", out.HomeRegion)
	}
	switch in.Source {
	case control.IdentityDefault, "":
	case control.IdentityKey:
		id := strings.TrimSpace(in.AccessKeyID)
		switch {
		case id == "":
			return awsCloudConfig{}, errors.New("enter the access key id")
		case strings.HasPrefix(id, "ASIA"):
			return awsCloudConfig{}, errors.New("that is a temporary access key, which stops working within hours; " +
				"create an access key for an IAM user instead")
		case !reAWSAccessKeyID.MatchString(id):
			return awsCloudConfig{}, errors.New("an access key id is 20 characters starting with AKIA")
		}
		out.AccessKeyID = id
		switch {
		case in.SecretAccessKey != nil && strings.TrimSpace(*in.SecretAccessKey) != "":
			secret := strings.TrimSpace(*in.SecretAccessKey)
			if len(secret) != 40 {
				return awsCloudConfig{}, errors.New("a secret access key is 40 characters")
			}
			if st == nil {
				out.SecretAccessKey = secret
				break
			}
			ref, err := st.write(awsSecretName, secret)
			if err != nil {
				return awsCloudConfig{}, err
			}
			out.SecretAccessKey = ref
		case prev.SecretAccessKey != "" && prev.AccessKeyID == id:
			// The configured secret belongs to this key id, and to no other.
			out.SecretAccessKey = prev.SecretAccessKey
		default:
			return awsCloudConfig{}, errors.New("enter the secret access key that goes with " + id)
		}
	case control.IdentityFile:
		out.Profile = strings.TrimSpace(in.Profile)
		out.CredentialsFile = strings.TrimSpace(in.CredentialsFile)
		if out.Profile == "" && out.CredentialsFile == "" {
			return awsCloudConfig{}, errors.New("name a profile, a credentials file, or both")
		}
		if f := out.CredentialsFile; f != "" {
			if !filepath.IsAbs(f) {
				return awsCloudConfig{}, fmt.Errorf("the credentials file must be a full path, such as /var/lib/argus-control/aws-credentials")
			}
			if err := secrets.CheckPrivate(f); err != nil {
				return awsCloudConfig{}, fmt.Errorf("the credentials file: %w", err)
			}
		}
	default:
		return awsCloudConfig{}, fmt.Errorf("unknown identity source %q", in.Source)
	}
	return out, nil
}

// TestCloud asks AWS who in would be, without saving anything.
func (b *settingsBackend) TestCloud(ctx context.Context, provider string, in control.CloudSettings) (control.CloudIdentityCheck, error) {
	if provider != "aws" {
		return control.CloudIdentityCheck{}, control.BadInput(fmt.Errorf("unknown cloud %q", provider))
	}
	// The running configuration, references resolved, so a secret the form
	// leaves out is the real one.
	var prev awsCloudConfig
	if b.cfg.Cloud != nil {
		prev = b.cfg.Cloud.AWS
	}
	cfg, err := awsConfigFrom(in.AWS, prev, nil)
	if err != nil {
		return control.CloudIdentityCheck{}, err
	}
	// The default chain on a host off AWS waits on the instance metadata
	// address; this bounds that wait as well as a slow STS.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	base, err := awscloud.New(cfg.options()).Base(ctx)
	if err != nil {
		return control.CloudIdentityCheck{}, err
	}
	return control.CloudIdentityCheck{Provider: provider, ARN: base.ARN, Principal: base.Principal, Source: base.Source}, nil
}
