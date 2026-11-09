package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Console and command-line sessions. Each is a fresh AssumeRole of the
level's console role, as the person, for no longer than they asked; the
console link comes from AWS's federation endpoint, which turns those
credentials into a one-time sign-in token. Nothing is cached or stored.
*/

// Console role names, as the template makes them.
const (
	ConsoleReadRoleName    = "ArgusConsoleRead"
	ConsoleOperateRoleName = "ArgusConsoleOperate"
)

// consolePolicy is the AWS managed policy each level's role has.
var consolePolicy = map[cloud.ConsoleLevel]string{
	cloud.ConsoleRead:    "arn:aws:iam::aws:policy/ReadOnlyAccess",
	cloud.ConsoleOperate: "arn:aws:iam::aws:policy/PowerUserAccess",
}

func consoleRoleName(l cloud.ConsoleLevel) string {
	if l == cloud.ConsoleOperate {
		return ConsoleOperateRoleName
	}
	return ConsoleReadRoleName
}

// ConsoleRole is the level's role in an account.
func (p *Provider) ConsoleRole(accountID string, level cloud.ConsoleLevel) string {
	return "arn:aws:iam::" + accountID + ":role/" + consoleRoleName(level)
}

// federationURL is AWS's sign-in federation endpoint; tests point it
// elsewhere.
var federationURL = "https://signin.aws.amazon.com/federation"

// consoleURL is where a sign-in lands.
const consoleURL = "https://console.aws.amazon.com/"

func (p *Provider) consoleCreds(ctx context.Context, a cloud.Account, actor cloud.Actor, level cloud.ConsoleLevel, d time.Duration) (*sts.AssumeRoleOutput, error) {
	if !actor.Valid() {
		return nil, fmt.Errorf("%q cannot be a source identity", actor)
	}
	if !cloud.ValidConsoleLevel(level) {
		return nil, fmt.Errorf("%q is not a console level", level)
	}
	d = min(max(d, cloud.MinConsoleSession), cloud.MaxConsoleSession)
	base, err := p.base(ctx)
	if err != nil {
		return nil, err
	}
	who := string(actor)
	out, err := sts.NewFromConfig(base).AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         awssdk.String(p.ConsoleRole(a.AccountID, level)),
		RoleSessionName: awssdk.String(sessionName(who)),
		SourceIdentity:  awssdk.String(who),
		ExternalId:      awssdk.String(a.ExternalID),
		DurationSeconds: awssdk.Int32(int32(d / time.Second)),
	})
	if err != nil {
		return nil, fmt.Errorf("could not open a %s session: %s. If the role is missing, update the argus-access stack from the account's row on the Cloud overview", level, plain(err))
	}
	return out, nil
}

// Credentials are the session's keys, for the command line.
func (p *Provider) Credentials(ctx context.Context, a cloud.Account, actor cloud.Actor, level cloud.ConsoleLevel, d time.Duration, region string) (cloud.Credentials, error) {
	out, err := p.consoleCreds(ctx, a, actor, level, d)
	if err != nil {
		return cloud.Credentials{}, err
	}
	c := out.Credentials
	return cloud.Credentials{AccessKeyID: awssdk.ToString(c.AccessKeyId), SecretAccessKey: awssdk.ToString(c.SecretAccessKey),
		SessionToken: awssdk.ToString(c.SessionToken), Expires: awssdk.ToTime(c.Expiration), Region: region}, nil
}

// ConsoleLink turns a session into a one-time sign-in link. issuer is
// where AWS sends the person when the session ends.
func (p *Provider) ConsoleLink(ctx context.Context, a cloud.Account, actor cloud.Actor, level cloud.ConsoleLevel, d time.Duration, issuer, region string) (cloud.ConsoleLink, error) {
	out, err := p.consoleCreds(ctx, a, actor, level, d)
	if err != nil {
		return cloud.ConsoleLink{}, err
	}
	c := out.Credentials
	sess, _ := json.Marshal(map[string]string{"sessionId": awssdk.ToString(c.AccessKeyId),
		"sessionKey": awssdk.ToString(c.SecretAccessKey), "sessionToken": awssdk.ToString(c.SessionToken)})
	// A chained session takes no SessionDuration; the console session ends
	// with the credentials.
	q := url.Values{"Action": {"getSigninToken"}, "Session": {string(sess)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, federationURL, nil)
	if err != nil {
		return cloud.ConsoleLink{}, err
	}
	req.URL.RawQuery = q.Encode()
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return cloud.ConsoleLink{}, fmt.Errorf("AWS sign-in did not answer: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return cloud.ConsoleLink{}, fmt.Errorf("AWS sign-in refused the session (%s)", resp.Status)
	}
	var tok struct {
		SigninToken string `json:"SigninToken"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.SigninToken == "" {
		return cloud.ConsoleLink{}, errors.New("AWS sign-in answered without a token")
	}
	dest := consoleURL
	if region != "" {
		dest += "?region=" + url.QueryEscape(region)
	}
	login := url.Values{"Action": {"login"}, "Issuer": {issuer}, "Destination": {dest}, "SigninToken": {tok.SigninToken}}
	return cloud.ConsoleLink{URL: federationURL + "?" + login.Encode(), Expires: awssdk.ToTime(c.Expiration)}, nil
}

// consoleRolesMissing names the console roles an account lacks, as
// "role:Name", read with the read role.
func consoleRolesMissing(ctx context.Context, c *iam.Client) ([]string, error) {
	var out []string
	for _, n := range []string{ConsoleReadRoleName, ConsoleOperateRoleName} {
		if _, err := c.GetRole(ctx, &iam.GetRoleInput{RoleName: awssdk.String(n)}); err != nil {
			var nf interface{ ErrorCode() string }
			if errors.As(err, &nf) && nf.ErrorCode() == "NoSuchEntity" {
				out = append(out, "role:"+n)
				continue
			}
			return nil, errors.New(plain(err))
		}
	}
	return out, nil
}

var _ cloud.Console = (*Provider)(nil)
