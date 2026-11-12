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

var (
	// An IAM user's long-lived key. ASIA keys are temporary, and expire.
	reAWSAccessKeyID = regexp.MustCompile(`^AKIA[A-Z0-9]{16}$`)
	reAWSRegion      = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	// An identity's id: what accounts store and what its secret file is
	// named after, so nothing that could leave the secrets directory.
	reIdentityID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// An organization id as Argus mints them, or the default's.
	reOrgID = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
)

// awsSecretName is the file under the secrets directory an identity's
// secret access key is written to.
func awsSecretName(id string) string { return "aws-" + id + "-secret-access-key" }

// cloudSettingsOf is the configured identities as the console shows them.
// c is the running configuration, references resolved, so a secret is only
// ever reported as set.
func cloudSettingsOf(c *cloudConfig) *control.CloudSettings {
	list, def := c.awsIdentities()
	out := &control.CloudSettings{AWS: control.AWSCloudSettings{Default: def,
		Identities: make([]control.AWSIdentitySettings, 0, len(list))}}
	for _, a := range list {
		orgs := a.Organizations
		if orgs == nil {
			orgs = []string{}
		}
		out.AWS.Identities = append(out.AWS.Identities, control.AWSIdentitySettings{
			ID: a.ID, Name: a.Name, Organizations: orgs, Source: a.source(),
			AccessKeyID: a.AccessKeyID, SecretAccessKeySet: a.SecretAccessKey != "",
			Profile: a.Profile, CredentialsFile: a.CredentialsFile, HomeRegion: a.HomeRegion,
		})
	}
	return out
}

// applyCloud writes in's identities into c. Nil, or a list that is not
// there at all (a console from before several identities), keeps what is
// configured. Secrets of identities removed or replaced are deleted once
// the save has worked.
func applyCloud(c *config, in *control.CloudSettings, st *secretStage) error {
	if in == nil || in.AWS.Identities == nil {
		return nil
	}
	if len(in.AWS.Identities) == 0 {
		return control.BadInput(errors.New("keep at least one AWS identity; argus-control starts from one to reach any account"))
	}
	if len(in.AWS.Identities) > control.MaxCloudIdentities {
		return control.BadInput(fmt.Errorf("at most %d AWS identities", control.MaxCloudIdentities))
	}
	prevList, _ := c.Cloud.awsIdentities()
	prev := map[string]awsIdentityConfig{}
	for _, p := range prevList {
		prev[p.ID] = p
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, i := range in.AWS.Identities {
		if id := strings.TrimSpace(i.ID); id != "" {
			ids[id] = true
		}
	}
	out := make([]awsIdentityConfig, 0, len(in.AWS.Identities))
	hosts := 0
	for _, i := range in.AWS.Identities {
		i.ID, i.Name = strings.TrimSpace(i.ID), strings.TrimSpace(i.Name)
		if i.Name == "" {
			return control.BadInput(errors.New("give every identity a name"))
		}
		if len(i.Name) > 64 {
			return control.BadInput(fmt.Errorf("%q: a name is at most 64 characters", i.Name))
		}
		if names[strings.ToLower(i.Name)] {
			return control.BadInput(fmt.Errorf("two identities are called %q; give each its own name", i.Name))
		}
		names[strings.ToLower(i.Name)] = true
		if i.ID == "" {
			i.ID = newIdentityID(i.Name, ids)
		} else if !reIdentityID.MatchString(i.ID) {
			return control.BadInput(fmt.Errorf("%q is not an identity id", i.ID))
		}
		next, err := awsIdentityFrom(i, prev[i.ID], st)
		if err != nil {
			return control.BadInput(fmt.Errorf("%s: %w", i.Name, err))
		}
		if next.source() == control.IdentityDefault {
			hosts++
		}
		if hosts > 1 {
			return control.BadInput(errors.New("only one identity can come from where argus-control runs: the host has only one"))
		}
		if p, ok := prev[i.ID]; ok && p.SecretAccessKey != "" && p.SecretAccessKey != next.SecretAccessKey {
			st.drop(p.SecretAccessKey)
		}
		out = append(out, next)
	}
	seen := map[string]bool{}
	for _, o := range out {
		if seen[o.ID] {
			return control.BadInput(fmt.Errorf("two identities have the id %q", o.ID))
		}
		seen[o.ID] = true
	}
	for id, p := range prev {
		if !seen[id] && p.SecretAccessKey != "" {
			st.drop(p.SecretAccessKey)
		}
	}
	// The default is an id, or the name of one being added, which has no
	// id until now.
	def := strings.TrimSpace(in.AWS.Default)
	if def != "" && !seen[def] {
		for _, o := range out {
			if strings.EqualFold(o.Name, def) {
				def = o.ID
				break
			}
		}
	}
	switch {
	case def == "":
		def = out[0].ID
	case !seen[def]:
		return control.BadInput(fmt.Errorf("the default identity %q is not in the list", def))
	}
	// The host's own identity alone, as Main, is what no cloud block means.
	if len(out) == 1 && out[0].ID == mainIdentity && out[0].Name == "Main" && out[0].source() == control.IdentityDefault &&
		out[0].HomeRegion == "" && len(out[0].Organizations) == 0 {
		c.Cloud = nil
		return nil
	}
	c.Cloud = &cloudConfig{AWS: awsCloudConfig{Default: def, Identities: out}}
	return nil
}

// newIdentityID makes a stable id from a name: lower case, letters and
// digits joined by dashes, unique among taken.
func newIdentityID(name string, taken map[string]bool) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r == '\'' || r == '\u2019':
			// "Northwind's" is northwinds, not northwind-s.
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		default:
			dash = true
		}
		if b.Len() >= 24 {
			break
		}
	}
	base := strings.Trim(b.String(), "-")
	if len(base) > 24 {
		base = strings.Trim(base[:24], "-")
	}
	if base == "" {
		base = "aws"
	}
	id := base
	for n := 2; taken[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	taken[id] = true
	return id
}

// awsIdentityFrom is in as configuration. prev is that identity as
// configured now, as written in the file: its secret is the reference,
// which a form that leaves the secret out keeps. With st nil the secret is
// used as typed, for a test that saves nothing.
func awsIdentityFrom(in control.AWSIdentitySettings, prev awsIdentityConfig, st *secretStage) (awsIdentityConfig, error) {
	out := awsIdentityConfig{ID: in.ID, Name: in.Name, HomeRegion: strings.TrimSpace(in.HomeRegion)}
	if out.HomeRegion != "" && !reAWSRegion.MatchString(out.HomeRegion) {
		return awsIdentityConfig{}, fmt.Errorf("%q is not an AWS region, such as us-east-1", out.HomeRegion)
	}
	if in.Organizations != nil {
		seen := map[string]bool{}
		for _, o := range in.Organizations {
			o = strings.TrimSpace(o)
			if !reOrgID.MatchString(o) {
				return awsIdentityConfig{}, fmt.Errorf("%q is not an organization", o)
			}
			if !seen[o] {
				seen[o] = true
				out.Organizations = append(out.Organizations, o)
			}
		}
	}
	switch in.Source {
	case control.IdentityDefault, "":
	case control.IdentityKey:
		id := strings.TrimSpace(in.AccessKeyID)
		switch {
		case id == "":
			return awsIdentityConfig{}, errors.New("enter the access key id")
		case strings.HasPrefix(id, "ASIA"):
			return awsIdentityConfig{}, errors.New("that is a temporary access key, which stops working within hours; " +
				"create an access key for an IAM user instead")
		case !reAWSAccessKeyID.MatchString(id):
			return awsIdentityConfig{}, errors.New("an access key id is 20 characters starting with AKIA")
		}
		out.AccessKeyID = id
		switch {
		case in.SecretAccessKey != nil && strings.TrimSpace(*in.SecretAccessKey) != "":
			secret := strings.TrimSpace(*in.SecretAccessKey)
			if len(secret) != 40 {
				return awsIdentityConfig{}, errors.New("a secret access key is 40 characters")
			}
			if st == nil {
				out.SecretAccessKey = secret
				break
			}
			ref, err := st.write(awsSecretName(in.ID), secret)
			if err != nil {
				return awsIdentityConfig{}, err
			}
			out.SecretAccessKey = ref
		case prev.SecretAccessKey != "" && prev.AccessKeyID == id:
			// The configured secret belongs to this key id, and to no other.
			out.SecretAccessKey = prev.SecretAccessKey
		default:
			return awsIdentityConfig{}, errors.New("enter the secret access key that goes with " + id)
		}
	case control.IdentityFile:
		out.Profile = strings.TrimSpace(in.Profile)
		out.CredentialsFile = strings.TrimSpace(in.CredentialsFile)
		if out.Profile == "" && out.CredentialsFile == "" {
			return awsIdentityConfig{}, errors.New("name a profile, a credentials file, or both")
		}
		if f := out.CredentialsFile; f != "" {
			if !filepath.IsAbs(f) {
				return awsIdentityConfig{}, fmt.Errorf("the credentials file must be a full path, such as /var/lib/argus-control/aws-credentials")
			}
			if err := secrets.CheckPrivate(f); err != nil {
				return awsIdentityConfig{}, fmt.Errorf("the credentials file: %w", err)
			}
		}
	default:
		return awsIdentityConfig{}, fmt.Errorf("unknown identity source %q", in.Source)
	}
	return out, nil
}

// TestCloud asks AWS who in would be, without saving anything. A saved
// identity's secret is the running one when the form leaves it out.
func (b *settingsBackend) TestCloud(ctx context.Context, provider string, in control.AWSIdentitySettings) (control.CloudIdentityCheck, error) {
	if provider != "aws" {
		return control.CloudIdentityCheck{}, control.BadInput(fmt.Errorf("unknown cloud %q", provider))
	}
	var prev awsIdentityConfig
	list, _ := b.cfg.Cloud.awsIdentities()
	for _, p := range list {
		if p.ID == in.ID {
			prev = p
		}
	}
	cfg, err := awsIdentityFrom(in, prev, nil)
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

// validateCloud refuses a hand-edited cloud block argus-control could not
// run on: ids that are not ids or repeat, a default that is not there, two
// identities from the host, too many.
func validateCloud(c *cloudConfig) error {
	if c == nil || len(c.AWS.Identities) == 0 {
		return nil
	}
	if len(c.AWS.Identities) > control.MaxCloudIdentities {
		return fmt.Errorf("cloud.aws: at most %d identities", control.MaxCloudIdentities)
	}
	seen, hosts := map[string]bool{}, 0
	for _, i := range c.AWS.Identities {
		if !reIdentityID.MatchString(i.ID) {
			return fmt.Errorf("cloud.aws.identities: %q is not an id: lower case letters, digits and dashes", i.ID)
		}
		if seen[i.ID] {
			return fmt.Errorf("cloud.aws.identities: the id %q is there twice", i.ID)
		}
		seen[i.ID] = true
		if i.source() == control.IdentityDefault {
			hosts++
		}
	}
	if hosts > 1 {
		return errors.New("cloud.aws.identities: only one identity can come from where argus-control runs")
	}
	if d := c.AWS.Default; d != "" && !seen[d] {
		return fmt.Errorf("cloud.aws.default: no identity has the id %q", d)
	}
	return nil
}
