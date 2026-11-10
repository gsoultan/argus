// Package aws is the AWS adapter under the neutral cloud layer.
//
// Argus never holds a key for a connected account. The control plane has one
// base identity: an access key an administrator entered under Settings ›
// Cloud identities (kept in an owner-only file, like every other secret in
// the config), or else whatever the SDK's usual chain finds on the control
// host (an instance profile, a container role, IAM Roles Anywhere through a
// credential_process profile, environment variables, or a credentials file
// named in the config and mode-checked like a private key). Every account is reached by assuming
// a role in it that trusts only that identity, requires an external id the
// control plane derives, and requires a source identity, so CloudTrail names
// the Argus user a call ran for.
package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/secrets"
)

// Options say where the base identity comes from. All empty is the SDK's
// default chain.
type Options struct {
	// AccessKeyID and SecretAccessKey are a long-lived IAM user key, entered
	// in Settings for a control plane that is not on AWS. When set they are
	// the identity, ahead of the profile, the file and the default chain.
	AccessKeyID     string
	SecretAccessKey string
	// Profile is a named profile in the shared config, for example one whose
	// credential_process is IAM Roles Anywhere's signing helper.
	Profile string
	// CredentialsFile is a shared credentials file. It is refused unless
	// only its owner can read it.
	CredentialsFile string
	// HomeRegion is where STS and DescribeRegions are asked. us-east-1
	// when empty; set it for another partition.
	HomeRegion string
}

// SessionDuration is how long one assumed-role session lasts. The shortest
// AWS allows: a cached session never outlives a quarter of an hour.
const SessionDuration = 15 * time.Minute

// maxSessions bounds the session cache. Keys come from connected accounts
// and signed-in people, not from request input, but a bound is the rule for
// any map that grows with use.
const maxSessions = 512

// Provider is AWS.
type Provider struct {
	opts Options

	once    sync.Once
	cfg     awssdk.Config
	cfgErr  error
	mu      sync.Mutex
	session map[sessionKey]*awssdk.CredentialsCache
}

type sessionKey struct {
	role, externalID string
	actor            cloud.Actor
}

// New returns the AWS provider. Nothing is read until it is first used.
func New(opts Options) *Provider {
	if opts.HomeRegion == "" {
		opts.HomeRegion = "us-east-1"
	}
	return &Provider{opts: opts, session: map[sessionKey]*awssdk.CredentialsCache{}}
}

func (p *Provider) Kind() string { return "aws" }

func (p *Provider) base(ctx context.Context) (awssdk.Config, error) {
	p.once.Do(func() {
		var lo []func(*config.LoadOptions) error
		lo = append(lo, config.WithRegion(p.opts.HomeRegion))
		if id := p.opts.AccessKeyID; id != "" {
			if p.opts.SecretAccessKey == "" {
				p.cfgErr = errors.New("an access key id is set without its secret access key")
				return
			}
			lo = append(lo, config.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(id, p.opts.SecretAccessKey, "")))
		}
		if p.opts.Profile != "" {
			lo = append(lo, config.WithSharedConfigProfile(p.opts.Profile))
		}
		if f := p.opts.CredentialsFile; f != "" {
			if err := secrets.CheckPrivate(f); err != nil {
				p.cfgErr = fmt.Errorf("the AWS credentials file: %w", err)
				return
			}
			lo = append(lo, config.WithSharedCredentialsFiles([]string{f}))
		}
		p.cfg, p.cfgErr = config.LoadDefaultConfig(ctx, lo...)
	})
	return p.cfg, p.cfgErr
}

// Base asks STS who this control plane is.
func (p *Provider) Base(ctx context.Context) (cloud.Base, error) {
	cfg, err := p.base(ctx)
	if err != nil {
		return cloud.Base{}, err
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return cloud.Base{}, fmt.Errorf("%w: %s", cloud.ErrNoBase, plain(err))
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return cloud.Base{}, fmt.Errorf("%w: %s", cloud.ErrNoBase, plain(err))
	}
	arn := awssdk.ToString(out.Arn)
	return cloud.Base{ARN: arn, Principal: TrustPrincipal(arn), Source: describeSource(creds.Source)}, nil
}

// TrustPrincipal is the ARN a trust policy names for the identity that
// answered as arn. A role's session, arn:aws:sts::1:assumed-role/Name/s, is
// trusted as the role itself, arn:aws:iam::1:role/Name. A role's path is
// not in a session ARN; a role under a path is named without it, which IAM
// accepts only for roles at the root, so give the control host a role at
// the root.
func TrustPrincipal(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[2] != "sts" {
		return arn
	}
	res := strings.Split(parts[5], "/")
	if len(res) < 2 || res[0] != "assumed-role" {
		return arn
	}
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", parts[1], parts[4], res[1])
}

func describeSource(src string) string {
	switch {
	case strings.HasPrefix(src, "EC2RoleProvider"):
		return "the instance profile of the EC2 instance argus-control runs on"
	case strings.Contains(src, "Endpoint"), strings.Contains(src, "ContainerCredentials"):
		return "the container's task role"
	case strings.HasPrefix(src, credentials.StaticCredentialsName):
		return "the access key in Settings › Cloud identities"
	case strings.HasPrefix(src, "EnvConfigCredentials"):
		return "AWS_ environment variables of argus-control"
	case strings.HasPrefix(src, "ProcessProvider"):
		return "a credential_process profile (for example IAM Roles Anywhere)"
	case strings.HasPrefix(src, "SharedConfigCredentials"):
		return "a shared credentials file"
	case strings.HasPrefix(src, "SSOProvider"):
		return "an IAM Identity Center profile"
	case src == "":
		return "the SDK's default chain"
	}
	return src
}

// assumed returns a config whose credentials are the account's role,
// assumed as actor. Sessions are cached per role, external id and actor,
// so one person's session never answers for another.
func (p *Provider) assumed(ctx context.Context, a cloud.Account, actor cloud.Actor, region string) (awssdk.Config, error) {
	if !actor.Valid() {
		return awssdk.Config{}, fmt.Errorf("%q cannot be a source identity: 2 to 64 letters, digits or +=,.@-_", actor)
	}
	if !ValidRoleARN(a.RoleARN) {
		return awssdk.Config{}, fmt.Errorf("%q is not an IAM role ARN", a.RoleARN)
	}
	base, err := p.base(ctx)
	if err != nil {
		return awssdk.Config{}, err
	}
	k := sessionKey{role: a.RoleARN, externalID: a.ExternalID, actor: actor}
	p.mu.Lock()
	cache, ok := p.session[k]
	if !ok {
		if len(p.session) >= maxSessions {
			p.session = map[sessionKey]*awssdk.CredentialsCache{}
		}
		ext, who := a.ExternalID, string(actor)
		prov := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(base), a.RoleARN, func(o *stscreds.AssumeRoleOptions) {
			o.ExternalID = &ext
			o.SourceIdentity = &who
			o.RoleSessionName = sessionName(who)
			o.Duration = SessionDuration
		})
		cache = awssdk.NewCredentialsCache(prov)
		p.session[k] = cache
	}
	p.mu.Unlock()
	cfg := base.Copy()
	cfg.Credentials = cache
	if region != "" {
		cfg.Region = region
	}
	return cfg, nil
}

// sessionName is the role session name: the actor where it fits, which it
// always does (both are 64 characters at most, with the same alphabet but
// for the session name lacking nothing the actor has).
func sessionName(actor string) string {
	n := "argus-" + actor
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// ValidRoleARN reports whether s looks like an IAM role ARN.
func ValidRoleARN(s string) bool {
	parts := strings.SplitN(s, ":", 6)
	return len(parts) == 6 && parts[0] == "arn" && parts[2] == "iam" && parts[3] == "" &&
		len(parts[4]) == 12 && strings.HasPrefix(parts[5], "role/") && len(parts[5]) > len("role/")
}

// Verify assumes the role as actor, checks it is in the account it should
// be, and lists the regions the account has enabled.
func (p *Provider) Verify(ctx context.Context, a cloud.Account, actor cloud.Actor) (cloud.Identity, error) {
	cfg, err := p.assumed(ctx, a, actor, p.opts.HomeRegion)
	if err != nil {
		return cloud.Identity{}, err
	}
	who, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return cloud.Identity{}, errors.New(plain(err))
	}
	if got := awssdk.ToString(who.Account); got != a.AccountID {
		return cloud.Identity{}, fmt.Errorf("the role is in account %s, not %s", got, a.AccountID)
	}
	regs, err := ec2.NewFromConfig(cfg).DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return cloud.Identity{}, fmt.Errorf("the role cannot list regions: %s", plain(err))
	}
	out := cloud.Identity{ARN: awssdk.ToString(who.Arn), AccountID: a.AccountID}
	for _, r := range regs.Regions {
		out.Regions = append(out.Regions, awssdk.ToString(r.RegionName))
	}
	return out, nil
}

// plain turns an SDK error into the sentence a person can act on: the
// service's code and message, without the operation's wrapping.
func plain(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, "api error "); i >= 0 {
		return s[i+len("api error "):]
	}
	return s
}
