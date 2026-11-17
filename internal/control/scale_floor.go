package control

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
A scale group's AWS side (cloud/floor.go): a launch template and an Auto
Scaling group, made by the scaler once the group's image is ready, so that
AWS keeps the group's minimum even when argus-control is down.

  - Argus decides (ask each time, automatic): the Auto Scaling group holds
    the minimum less the server the group was made from, no more and no
    less. Growing above it is Argus's own launches, as before, and those are
    what Argus takes away.
  - AWS decides: the Auto Scaling group runs between the minimum and the
    maximum (less the source) with a target-tracking policy, and Argus only
    records what AWS did.

A server AWS starts names itself <group>-<instance id> and enrolls its agent
with the group's key. The key sits in the launch template, so it is not a
secret from anyone who can read the template; what it can do is limited to
enrolling, under that name, a server AWS really started in that group in
the last 30 minutes that has not enrolled yet (checkScaleKey).
*/

// floorCopies is how many copies the Auto Scaling group keeps at least:
// the minimum, less the server the group was made from.
func floorCopies(g ScaleGroup) int32 {
	n := int32(g.Limits.Min - 1)
	if n < 0 {
		return 0
	}
	return n
}

// floorCount is the Auto Scaling group's size for g, given how many it
// wants now.
func floorCount(g ScaleGroup, desired int32) cloud.GroupSize {
	lo := floorCopies(g)
	if g.Mode != cloud.ScaleAWS {
		// AWS keeps exactly the floor; a maximum of at least 1 is AWS's rule.
		return cloud.GroupSize{Min: lo, Max: max(lo, 1), Desired: lo}
	}
	hi := max(int32(g.Limits.Max-1), lo, 1)
	return cloud.GroupSize{Min: lo, Max: hi, Desired: min(max(desired, lo), hi)}
}

// floorDesired is how many the Auto Scaling group wants now, as last read.
func (a *API) floorDesired(ctx context.Context, g ScaleGroup) (int32, bool) {
	if g.AWSGroup == "" {
		return 0, false
	}
	r, err := a.store.CloudResourceOf(ctx, g.AccountID, g.Region, cloud.KindScalingGroup, g.AWSGroup)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(r.Details["desired"])
	return int32(n), err == nil
}

// floorNaming names a server after its instance, from the instance
// metadata, for its agent to enroll as.
func floorNaming(group string) string {
	return "t=$(curl -fsS -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)\n" +
		"id=$(curl -fsS -H \"X-aws-ec2-metadata-token: $t\" http://169.254.169.254/latest/meta-data/instance-id)\n" +
		"case \"$id\" in i-[0-9a-f]*) ;; *) echo 'Argus: no instance id from the metadata service' >&2; exit 1 ;; esac\n" +
		"h='" + group + "-'\"$id\"\n" +
		"hostnamectl set-hostname \"$h\" 2>/dev/null || hostname \"$h\"\n"
}

// floorLabel is CloudWatch's name for the group's load balancer and target
// group, for tracking requests, or "".
func (a *API) floorLabel(ctx context.Context, g ScaleGroup) string {
	tg, err := a.store.CloudResourceOf(ctx, g.AccountID, g.Region, cloud.KindTargetGroup, g.TargetGroup)
	if err != nil {
		return ""
	}
	for _, l := range tg.Links {
		if l.Kind == cloud.KindLoadBalancer {
			if label := cloud.RequestLabel(l.ID, tg.ID); label != "" {
				return label
			}
		}
	}
	return ""
}

// floorPolicy is the target-tracking policy for g, on while AWS decides
// and the group is active.
func (a *API) floorPolicy(ctx context.Context, g ScaleGroup) cloud.FloorPolicy {
	t := g.AWSTrack
	if t.Metric == "" {
		t = cloud.DefaultAWSTrack()
	}
	return cloud.FloorPolicy{Track: t, Label: a.floorLabel(ctx, g), On: g.Mode == cloud.ScaleAWS && g.State == "active"}
}

// ensureFloor makes the launch template and Auto Scaling group under a
// group that has none, once its image is ready. AWS refusing is noted on
// the group and tried again after a while.
func (a *API) ensureFloor(ctx context.Context, m cloudManaged, g *ScaleGroup, now time.Time) {
	fl, ok := m.manager.(cloud.Floors)
	if !ok || g.AWSGroup != "" || g.Template.Image == "" {
		return
	}
	if g.AWSTriedAt != nil && g.AWSNote != "" && now.Sub(*g.AWSTriedAt) < cloud.FloorRetry {
		return
	}
	if img, err := a.store.CloudResourceOf(ctx, g.AccountID, g.Region, cloud.KindImage, g.Template.Image); err == nil &&
		img.State != "" && img.State != "available" {
		return
	}
	refused := func(why string) {
		note := "AWS could not make " + cloud.FloorName(g.Name) + ": " + why + ". Argus tries again in 30 minutes; " +
			"if AWS lacks a permission, update Argus's access on the account's row under Cloud"
		g.AWSNote, g.AWSTriedAt = note, &now
		_ = a.store.SetScaleFloor(ctx, g.ID, "", "", note, now)
		_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: "failed", Actor: "argus", Detail: note})
		a.auditScale(ctx, "floor-failed", g.Name, note+".")
	}
	key, err := a.store.CreateScaleKey(ctx, g.ID, string(cloud.ScaleActor))
	if err != nil {
		refused("Argus could not make the group's enrollment key (" + err.Error() + ")")
		return
	}
	ud, problem := a.agentUserDataNaming(a.scaleRequest(ctx, *g), floorNaming(g.Name), key)
	if problem != "" {
		refused(problem)
		return
	}
	tags := map[string]string{"argus:scale-group": g.Name, "argus:launched-by": "aws-auto-scaling"}
	for k, v := range g.Template.Tags {
		tags[k] = v
	}
	spec := cloud.FloorSpec{Name: cloud.FloorName(g.Name), Template: cloud.FloorName(g.Name) + "-" + now.Format("20060102-1504"),
		Image: g.Template.Image, Size: g.Template.Size, DiskGB: g.Template.DiskGB, Subnets: g.Template.Subnets,
		Firewalls: g.Template.Firewalls, Tags: tags, UserData: ud, TargetGroup: g.TargetGroup, Count: floorCount(*g, 0)}
	res, err := fl.CreateFloor(ctx, m.target, cloud.ScaleActor, g.Region, spec)
	if err != nil {
		refused(err.Error())
		return
	}
	g.AWSGroup, g.AWSTemplate, g.AWSNote, g.AWSTriedAt = res.ResourceID, res.Detail, "", &now
	_ = a.store.SetScaleFloor(ctx, g.ID, g.AWSGroup, g.AWSTemplate, "", now)
	detail := fmt.Sprintf("Made Auto Scaling group %s from launch template %s: AWS keeps %s even when Argus is down",
		g.AWSGroup, g.AWSTemplate, copiesWords(spec.Count.Min, g.Template.FromName))
	if g.Mode == cloud.ScaleAWS {
		p := a.floorPolicy(ctx, *g)
		if _, err := fl.SetFloorPolicy(ctx, m.target, cloud.ScaleActor, g.Region, g.AWSGroup, p); err != nil {
			detail += ". AWS refused its policy (" + err.Error() + "), so it does not grow yet; change who decides again to retry"
		} else {
			detail = fmt.Sprintf("Made Auto Scaling group %s from launch template %s: AWS keeps %s and decides the rest, keeping %s",
				g.AWSGroup, g.AWSTemplate, copiesWords(spec.Count.Min, g.Template.FromName), p.Track.Words())
		}
	}
	_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: "aws", Actor: "argus", Detail: detail})
	a.auditScale(ctx, "floor", g.Name, detail+"; AWS request "+res.RequestID+".")
	a.syncCloudAccountSoon(ctx, m.acct)
}

// copiesWords is "2 copies besides web-01", or "none besides web-01".
func copiesWords(n int32, source string) string {
	if n == 0 {
		return "no copies besides " + source + " (the minimum is " + source + " alone)"
	}
	return fmt.Sprintf("%d cop%s besides %s", n, map[bool]string{true: "y", false: "ies"}[n == 1], source)
}

// syncFloorMembers adds the servers AWS started in the group's Auto
// Scaling group, as the last inventory read shows them.
func (a *API) syncFloorMembers(ctx context.Context, g *ScaleGroup) {
	if g.AWSGroup == "" {
		return
	}
	known := map[string]bool{}
	for _, m := range g.Members {
		known[m.InstanceID] = true
	}
	items, err := a.store.CloudResources(ctx, cloud.KindInstance)
	if err != nil {
		return
	}
	added := false
	for _, it := range items {
		r := it.Resource
		if it.AccountID != g.AccountID || r.Region != g.Region || r.Tags["aws:autoscaling:groupName"] != g.AWSGroup || known[r.ID] {
			continue
		}
		if r.State == "terminated" || r.State == "shutting-down" {
			continue
		}
		mem := ScaleMember{InstanceID: r.ID, Hostname: g.Name + "-" + r.ID, Zone: r.Zone, State: "joining", AddedBy: "aws", InAWS: true}
		for _, l := range r.Links {
			if l.Kind == cloud.KindSubnet {
				mem.Subnet = l.ID
			}
		}
		// AWS puts it in the target group itself.
		at := time.Now().UTC()
		mem.RegisteredAt = &at
		if err := a.store.AddScaleMember(ctx, g.ID, mem); err != nil {
			continue
		}
		why := "to keep the minimum"
		if g.Mode == cloud.ScaleAWS {
			why = "as its policy decided"
		}
		_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: "added", Actor: "aws",
			Detail: "AWS started " + mem.Hostname + " in " + g.AWSGroup + " " + why + "; it serves once its health check passes"})
		added = true
	}
	if added {
		if fresh, err := a.store.ScaleGroup(ctx, g.ID); err == nil {
			*g = fresh
		}
	}
}

// floorChange sets the Auto Scaling group to g's settings after a change:
// its size, and its policy on or off. was is the mode before.
func (a *API) floorChange(ctx context.Context, m cloudManaged, actor cloud.Actor, g ScaleGroup, was string) error {
	if g.AWSGroup == "" {
		return nil
	}
	bal, ok := m.manager.(cloud.Balancing)
	fl, ok2 := m.manager.(cloud.Floors)
	if !ok || !ok2 {
		return errors.New(m.acct.Provider + " cannot change Auto Scaling groups from Argus")
	}
	desired, _ := a.floorDesired(ctx, g)
	if _, err := bal.SetGroupSize(ctx, m.target, actor, g.Region, g.AWSGroup, floorCount(g, desired)); err != nil {
		return fmt.Errorf("sizing %s: %w", g.AWSGroup, err)
	}
	if g.Mode == cloud.ScaleAWS || was == cloud.ScaleAWS {
		if _, err := fl.SetFloorPolicy(ctx, m.target, actor, g.Region, g.AWSGroup, a.floorPolicy(ctx, g)); err != nil {
			return fmt.Errorf("the policy on %s: %w", g.AWSGroup, err)
		}
	}
	a.syncCloudAccountSoon(ctx, m.acct)
	return nil
}

// argusCopies are the servers Argus launched itself and still serve: not
// the source, not AWS's.
func (g ScaleGroup) argusCopies() []ScaleMember {
	var out []ScaleMember
	for _, m := range g.serving() {
		if !m.InAWS && m.InstanceID != g.Template.From {
			out = append(out, m)
		}
	}
	return out
}

// awsServing counts the members AWS started that count towards the size.
func (g ScaleGroup) awsServing() int {
	n := 0
	for _, m := range g.serving() {
		if m.InAWS {
			n++
		}
	}
	return n
}

var reInstanceID = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)

// checkScaleKey says why a group's key may not enroll hostname, or nil:
// it must be <group>-<instance id> of a server AWS started in the group's
// Auto Scaling group within the last 30 minutes. That it has not enrolled
// already is the credential's own rule.
func (a *API) checkScaleKey(ctx context.Context, groupID, hostname string) error {
	g, err := a.store.ScaleGroup(ctx, groupID)
	if err != nil {
		return ErrEnrollTokenInvalid
	}
	inst, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(hostname)), g.Name+"-")
	if !ok || !reInstanceID.MatchString(inst) {
		return fmt.Errorf("scale group %s's key enrolls only servers named %s-<instance id>", g.Name, g.Name)
	}
	if g.AWSGroup == "" {
		return errors.New("scale group " + g.Name + " has no Auto Scaling group for its key")
	}
	m, _, err := a.cloudManagedAccount(ctx, g.AccountID)
	if err != nil {
		return errors.New("Argus cannot check " + hostname + " with AWS: " + err.Error())
	}
	fl, ok := m.manager.(cloud.Floors)
	if !ok {
		return errors.New("Argus cannot check " + hostname + " with " + m.acct.Provider)
	}
	fi, err := fl.FloorInstance(ctx, m.target, cloud.ScaleActor, g.Region, inst)
	if err != nil {
		return errors.New("Argus cannot check " + hostname + " with AWS: " + err.Error())
	}
	if fi.Group != g.AWSGroup {
		return errors.New(hostname + " is not a server AWS started for scale group " + g.Name)
	}
	if fi.State != "pending" && fi.State != "running" {
		return errors.New(hostname + " is " + fi.State)
	}
	if time.Since(fi.LaunchedAt) > cloud.FloorEnrollWindow {
		return errors.New(hostname + " started more than 30 minutes ago, too long for the group's key; enroll it by hand under Servers")
	}
	return nil
}

// floorWords is the AWS side in words, for the group's page.
func floorWords(g ScaleGroup) string {
	switch {
	case g.AWSGroup == "" && g.AWSNote != "":
		return g.AWSNote
	case g.AWSGroup == "":
		return "Argus makes the Auto Scaling group once the image is ready"
	case g.Mode == cloud.ScaleAWS:
		return "AWS decides, keeping " + g.AWSTrack.Words()
	}
	return "AWS keeps " + copiesWords(floorCopies(g), g.Template.FromName)
}
