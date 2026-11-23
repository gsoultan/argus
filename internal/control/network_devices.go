package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Network devices: a switch, router, firewall, printer, UPS or access point,
read over SNMP (plans/general-layout/design.md, C4). The second kind of check
on the shared engine (checks.go): a row of checks of kind snmp, scheduled,
leased, recorded and alerted like a website check.

Devices sit on private networks argus-control cannot reach, so a device is
always read by the agent of one to three servers near it (confirm_from), over
the generic checkrun tunnel service (tunnel/checkrun.go). The first server
that gets an answer decides; another server getting one when the first did
not is "partial", as for websites.

What counts as down: no answer from any of them, a refused request (wrong
credentials), or a watched interface not up, over two rounds as for every
kind. A restart (uptime going back) is told in the bell but is not an outage.

Credentials (community, v3 passwords) are sealed with a key derived from
signing_secret and never returned; the form keeps them when left empty.

	GET    /api/v1/network-devices              every device someone can see, as websites
	POST   /api/v1/network-devices              add one
	POST   /api/v1/network-devices/test         read a device once through its first server, saving nothing
	GET    /api/v1/network-devices/{id}         one device, its last day of rounds and its outages
	PUT    /api/v1/network-devices/{id}         change one
	DELETE /api/v1/network-devices/{id}         remove one, and its history
	POST   /api/v1/network-devices/{id}/check   check it now
	POST   /api/v1/network-devices/{id}/pause   pause or resume it
*/

// NetworkDevice is one device Argus reads over SNMP.
type NetworkDevice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Version   string `json:"version"`
	User      string `json:"user"`
	AuthProto string `json:"authProto"`
	PrivProto string `json:"privProto"`
	// Watch are the interfaces that must be up.
	Watch []WatchedInterface `json:"watch"`
	// HasSecret says credentials are saved; they are never returned.
	HasSecret      bool      `json:"hasSecret"`
	EverySeconds   int       `json:"everySeconds"`
	TimeoutSeconds int       `json:"timeoutSeconds"`
	CheckFrom      []string  `json:"checkFrom"`
	Paused         bool      `json:"paused"`
	GroupID        string    `json:"groupId"`
	CreatedBy      string    `json:"createdBy"`
	CreatedAt      time.Time `json:"createdAt"`
	secretsSealed  []byte

	State         string     `json:"state"`
	StateSince    *time.Time `json:"stateSince,omitempty"`
	FailingRounds int        `json:"failingRounds"`
	LastAt        *time.Time `json:"lastAt,omitempty"`
	LastMS        *int       `json:"lastMs,omitempty"`
	LastDetail    string     `json:"lastDetail"`
	LastOutcome   string     `json:"lastOutcome"`
	// Device is what it said about itself on its last answer.
	Device *DeviceInfo `json:"device,omitempty"`

	// Filled in for the console.
	Uptime     WebsiteUptime  `json:"uptime"`
	OpenOutage *WebsiteOutage `json:"openOutage,omitempty"`
	CheckHosts []string       `json:"checkHosts"`
	CanOperate bool           `json:"canOperate"`
	// Limited leaves out how it is read (user, protocols, servers), for
	// someone outside the fleet roles.
	Limited bool `json:"limited,omitempty"`
}

// WatchedInterface is an interface that must be up, by the device's index,
// with the name it had when it was chosen.
type WatchedInterface struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
}

// DeviceInfo is a device's last answer, kept in checks.last_data.
type DeviceInfo struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Location    string    `json:"location,omitempty"`
	UptimeSec   int64     `json:"uptimeSec"`
	At          time.Time `json:"at"`
	// RestartedAt is when it last came back from a restart, as its uptime
	// going back showed.
	RestartedAt *time.Time        `json:"restartedAt,omitempty"`
	Interfaces  []DeviceInterface `json:"interfaces"`
}

// DeviceInterface is an interface on the last answer, with its traffic
// since the answer before, in bits a second.
type DeviceInterface struct {
	tunnel.SNMPInterface
	InBps  *float64 `json:"inBps,omitempty"`
	OutBps *float64 `json:"outBps,omitempty"`
	// ErrorsUp is how many errors it counted since the answer before.
	ErrorsUp uint64 `json:"errorsUp,omitempty"`
	Watched  bool   `json:"watched,omitempty"`
}

// NetworkDeviceInput is the form.
type NetworkDeviceInput struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Version   string `json:"version"`
	User      string `json:"user"`
	AuthProto string `json:"authProto"`
	PrivProto string `json:"privProto"`
	// The credentials. Left empty on a saved device, the saved ones stay.
	Community      string             `json:"community"`
	AuthPass       string             `json:"authPass"`
	PrivPass       string             `json:"privPass"`
	Watch          []WatchedInterface `json:"watch"`
	EverySeconds   int                `json:"everySeconds"`
	TimeoutSeconds int                `json:"timeoutSeconds"`
	CheckFrom      []string           `json:"checkFrom"`
	Paused         bool               `json:"paused"`

	secretsSealed []byte
}

// deviceSecrets are a device's credentials, as sealed.
type deviceSecrets struct {
	Community string `json:"community,omitempty"`
	AuthPass  string `json:"authPass,omitempty"`
	PrivPass  string `json:"privPass,omitempty"`
}

// ErrNoDevice is a device that does not exist in this organization.
var ErrNoDevice = errors.New("no such network device")

func (in *NetworkDeviceInput) normalise() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	in.User = strings.TrimSpace(in.User)
	in.AuthProto = strings.ToUpper(strings.TrimSpace(in.AuthProto))
	in.PrivProto = strings.ToUpper(strings.TrimSpace(in.PrivProto))
	if in.Version == "" {
		in.Version = tunnel.SNMPv3
	}
	if in.Port == 0 {
		in.Port = 161
	}
	if in.Name == "" {
		in.Name = in.Host
	}
	if in.EverySeconds == 0 {
		in.EverySeconds = 60
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = tunnel.SNMPTimeoutDefault
	}
	if in.Version == tunnel.SNMPv2c {
		in.User, in.AuthProto, in.PrivProto, in.AuthPass, in.PrivPass = "", "", "", "", ""
	} else {
		in.Community = ""
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, id := range in.CheckFrom {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	in.CheckFrom = ids
	watch := []WatchedInterface{}
	seenIf := map[int]bool{}
	for _, w := range in.Watch {
		if w.Index > 0 && !seenIf[w.Index] {
			seenIf[w.Index] = true
			w.Name = clip(strings.TrimSpace(w.Name), 120)
			watch = append(watch, w)
		}
	}
	in.Watch = watch
	switch {
	case in.Host == "":
		return ErrInvalidWebsite{"enter the device's address"}
	case len(in.Name) > 100:
		return ErrInvalidWebsite{"the name is longer than 100 characters"}
	case in.EverySeconds != 60 && in.EverySeconds != 300 && in.EverySeconds != 600:
		return ErrInvalidWebsite{"check every 1, 5 or 10 minutes"}
	case in.TimeoutSeconds < 5 || in.TimeoutSeconds > tunnel.SNMPTimeoutMax:
		return ErrInvalidWebsite{fmt.Sprintf("the timeout must be between 5 and %d seconds", tunnel.SNMPTimeoutMax)}
	case len(in.CheckFrom) == 0:
		return ErrInvalidWebsite{"choose a server on the device's network to read it from"}
	case len(in.CheckFrom) > 3:
		return ErrInvalidWebsite{"choose at most three servers to read it from"}
	case len(in.Watch) > tunnel.SNMPInterfacesMax:
		return ErrInvalidWebsite{fmt.Sprintf("watch at most %d interfaces", tunnel.SNMPInterfacesMax)}
	}
	return nil
}

// query is what the agent is sent, credentials included.
func (in NetworkDeviceInput) query() tunnel.SNMPQuery {
	q := tunnel.SNMPQuery{Host: in.Host, Port: in.Port, Version: in.Version, Community: in.Community,
		User: in.User, AuthProto: in.AuthProto, AuthPass: in.AuthPass, PrivProto: in.PrivProto, PrivPass: in.PrivPass,
		TimeoutSec: in.TimeoutSeconds}
	for _, w := range in.Watch {
		q.Watch = append(q.Watch, w.Index)
	}
	return q
}

/* ── Credentials ─────────────────────────────────────────────────────────── */

func (a *API) deviceKey(ctx context.Context) ([]byte, error) {
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return nil, ErrInvalidWebsite{"this control plane cannot keep SNMP credentials safe: set signing_secret in its configuration"}
	}
	sum := sha256.Sum256(append([]byte("argus network device secrets v1\x00"), a.Tunnels.grantKeyFor(ctx).Seed()...))
	return sum[:], nil
}

func (a *API) sealDeviceSecrets(ctx context.Context, s deviceSecrets) ([]byte, error) {
	if s == (deviceSecrets{}) {
		return nil, nil
	}
	key, err := a.deviceKey(ctx)
	if err != nil {
		return nil, err
	}
	plain, _ := json.Marshal(s)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte("network-device-secrets")), nil
}

var errDeviceSecrets = errors.New("the device's SNMP credentials cannot be read; was signing_secret changed? Enter them again")

func (a *API) openDeviceSecrets(ctx context.Context, sealed []byte) (deviceSecrets, error) {
	var s deviceSecrets
	if len(sealed) == 0 {
		return s, nil
	}
	key, err := a.deviceKey(ctx)
	if err != nil {
		return s, errDeviceSecrets
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(sealed) < gcm.NonceSize() {
		return s, errDeviceSecrets
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte("network-device-secrets"))
	if err != nil || json.Unmarshal(plain, &s) != nil {
		return deviceSecrets{}, errDeviceSecrets
	}
	return s, nil
}

// prepareDevice fills the credentials a saved device keeps, checks the
// query is one an agent can send, and seals them. before is nil for a new
// device.
func (a *API) prepareDevice(ctx context.Context, in *NetworkDeviceInput, before *NetworkDevice) error {
	if err := in.normalise(); err != nil {
		return err
	}
	if before != nil && len(before.secretsSealed) > 0 {
		saved, err := a.openDeviceSecrets(ctx, before.secretsSealed)
		if err != nil {
			return ErrInvalidWebsite{err.Error()}
		}
		// Kept only for the same way in: a changed version or user means
		// the old password is not this one's.
		same := in.Version == before.Version && in.User == before.User
		if in.Community == "" && same {
			in.Community = saved.Community
		}
		if in.AuthPass == "" && same && in.AuthProto == before.AuthProto {
			in.AuthPass = saved.AuthPass
		}
		if in.PrivPass == "" && same && in.PrivProto == before.PrivProto {
			in.PrivPass = saved.PrivPass
		}
	}
	if err := in.query().Validate(); err != nil {
		return ErrInvalidWebsite{err.Error()}
	}
	sealed, err := a.sealDeviceSecrets(ctx, deviceSecrets{Community: in.Community, AuthPass: in.AuthPass, PrivPass: in.PrivPass})
	if err != nil {
		return err
	}
	in.secretsSealed = sealed
	return nil
}

// deviceQuery is what a round sends, its credentials opened.
func (a *API) deviceQuery(ctx context.Context, d NetworkDevice) (tunnel.SNMPQuery, error) {
	s, err := a.openDeviceSecrets(ctx, d.secretsSealed)
	in := NetworkDeviceInput{Host: d.Host, Port: d.Port, Version: d.Version, User: d.User, AuthProto: d.AuthProto,
		PrivProto: d.PrivProto, Community: s.Community, AuthPass: s.AuthPass, PrivPass: s.PrivPass,
		TimeoutSeconds: d.TimeoutSeconds, Watch: d.Watch}
	return in.query(), err
}

/* ── Store ───────────────────────────────────────────────────────────────── */

const deviceCols = `id::text, name, COALESCE(settings->>'host', ''), COALESCE((settings->>'port')::int, 161),
	COALESCE(settings->>'version', 'v3'), COALESCE(settings->>'user', ''), COALESCE(settings->>'authProto', ''),
	COALESCE(settings->>'privProto', ''), COALESCE(settings->'watch', '[]'::jsonb), secrets_sealed,
	every_seconds, timeout_seconds, confirm_from::text[], paused, COALESCE(group_id::text, ''), created_by, created_at,
	state, state_since, failing_rounds, last_at, last_ms, last_detail, last_outcome, last_data`

// deviceSettings is the settings column, from the numbered parameters.
const deviceSettings = `jsonb_build_object('host', $2::text, 'port', $3::int, 'version', $4::text, 'user', $5::text,
	'authProto', $6::text, 'privProto', $7::text, 'watch', $8::jsonb)`

func scanDevice(row pgx.Row) (NetworkDevice, error) {
	var d NetworkDevice
	var watch, last []byte
	err := row.Scan(&d.ID, &d.Name, &d.Host, &d.Port, &d.Version, &d.User, &d.AuthProto, &d.PrivProto, &watch, &d.secretsSealed,
		&d.EverySeconds, &d.TimeoutSeconds, &d.CheckFrom, &d.Paused, &d.GroupID, &d.CreatedBy, &d.CreatedAt,
		&d.State, &d.StateSince, &d.FailingRounds, &d.LastAt, &d.LastMS, &d.LastDetail, &d.LastOutcome, &last)
	if err != nil {
		return d, err
	}
	d.Watch = []WatchedInterface{}
	_ = json.Unmarshal(watch, &d.Watch)
	var info DeviceInfo
	if len(last) > 2 && json.Unmarshal(last, &info) == nil && !info.At.IsZero() {
		d.Device = &info
	}
	if d.CheckFrom == nil {
		d.CheckFrom = []string{}
	}
	d.CheckHosts = []string{}
	d.HasSecret = len(d.secretsSealed) > 0
	return d, nil
}

func scanDevices(rows pgx.Rows, err error) ([]NetworkDevice, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NetworkDevice{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// NetworkDevices lists every device, by name.
func (s *Store) NetworkDevices(ctx context.Context) ([]NetworkDevice, error) {
	return scanDevices(s.db(ctx).Query(ctx, `SELECT `+deviceCols+` FROM checks WHERE kind = 'snmp' ORDER BY lower(name), id`))
}

// NetworkDevice is one device.
func (s *Store) NetworkDevice(ctx context.Context, id string) (NetworkDevice, error) {
	d, err := scanDevice(s.db(ctx).QueryRow(ctx, `SELECT `+deviceCols+` FROM checks WHERE id::text = $1 AND kind = 'snmp'`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNoDevice
	}
	return d, err
}

// CreateNetworkDevice adds a device, due at once. in is prepared.
func (s *Store) CreateNetworkDevice(ctx context.Context, in NetworkDeviceInput, by string) (NetworkDevice, error) {
	if err := s.confirmServersExist(ctx, in.CheckFrom); err != nil {
		return NetworkDevice{}, err
	}
	var n int
	if err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM checks`).Scan(&n); err != nil {
		return NetworkDevice{}, err
	}
	if n >= websiteMaxChecks {
		return NetworkDevice{}, ErrInvalidWebsite{fmt.Sprintf("an organization can have at most %d checks", websiteMaxChecks)}
	}
	watch, _ := json.Marshal(in.Watch)
	return scanDevice(s.db(ctx).QueryRow(ctx, `INSERT INTO checks
		(kind, name, settings, secrets_sealed, every_seconds, timeout_seconds, confirm_from, paused, created_by, telegram)
		VALUES ('snmp', $1, `+deviceSettings+`, $9, $10, $11, $12::uuid[], $13, $14, 'default')
		RETURNING `+deviceCols,
		in.Name, in.Host, in.Port, in.Version, in.User, in.AuthProto, in.PrivProto, string(watch),
		in.secretsSealed, in.EverySeconds, in.TimeoutSeconds, in.CheckFrom, in.Paused, strings.ToLower(by)))
}

// UpdateNetworkDevice changes a device. in is prepared. A changed address
// starts over, as for a website.
func (s *Store) UpdateNetworkDevice(ctx context.Context, id string, in NetworkDeviceInput) (before, after NetworkDevice, err error) {
	if err := s.confirmServersExist(ctx, in.CheckFrom); err != nil {
		return before, after, err
	}
	if before, err = s.NetworkDevice(ctx, id); err != nil {
		return before, after, err
	}
	watch, _ := json.Marshal(in.Watch)
	moved := before.Host != in.Host || before.Port != in.Port
	after, err = scanDevice(s.db(ctx).QueryRow(ctx, `UPDATE checks SET name = $1, settings = `+deviceSettings+`,
		secrets_sealed = $9, every_seconds = $10, timeout_seconds = $11, confirm_from = $12::uuid[], paused = $13,
		updated_at = now(), next_at = now(),
		state = CASE WHEN $15 THEN 'pending' ELSE state END,
		state_since = CASE WHEN $15 THEN NULL ELSE state_since END,
		failing_rounds = CASE WHEN $15 THEN 0 ELSE failing_rounds END,
		last_data = CASE WHEN $15 THEN '{}'::jsonb ELSE last_data END
		WHERE id::text = $14 AND kind = 'snmp' RETURNING `+deviceCols,
		in.Name, in.Host, in.Port, in.Version, in.User, in.AuthProto, in.PrivProto, string(watch),
		in.secretsSealed, in.EverySeconds, in.TimeoutSeconds, in.CheckFrom, in.Paused, id, moved))
	if errors.Is(err, pgx.ErrNoRows) {
		return before, after, ErrNoDevice
	}
	if err == nil && moved {
		if _, err = s.db(ctx).Exec(ctx, `UPDATE check_outages SET ended_at = now() WHERE check_id::text = $1 AND ended_at IS NULL`, id); err != nil {
			return before, after, err
		}
	}
	return before, after, err
}

// DeleteNetworkDevice removes a device and its history.
func (s *Store) DeleteNetworkDevice(ctx context.Context, id string) (NetworkDevice, error) {
	d, err := scanDevice(s.db(ctx).QueryRow(ctx, `DELETE FROM checks WHERE id::text = $1 AND kind = 'snmp' RETURNING `+deviceCols, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNoDevice
	}
	return d, err
}

// TakeDueNetworkDevices leases up to limit due devices to this replica.
func (s *Store) TakeDueNetworkDevices(ctx context.Context, limit int) ([]NetworkDevice, error) {
	return scanDevices(s.db(ctx).Query(ctx, `UPDATE checks SET lease_until = now() + $2::interval
		WHERE id IN (SELECT id FROM checks
		              WHERE kind = 'snmp' AND NOT paused AND next_at <= now() AND (lease_until IS NULL OR lease_until < now())
		              ORDER BY next_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING `+deviceCols, limit, fmt.Sprintf("%d seconds", int(websiteLease.Seconds()))))
}

// CheckNow makes a check of any kind due at once.
func (s *Store) CheckNow(ctx context.Context, id string) error {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE checks SET next_at = now() WHERE id::text = $1 AND NOT paused`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNoDevice
	}
	return err
}

// SetCheckPaused pauses or resumes a check of any kind and returns its name.
func (s *Store) SetCheckPaused(ctx context.Context, id string, paused bool) (string, error) {
	var name string
	err := s.db(ctx).QueryRow(ctx, `UPDATE checks SET paused = $2, updated_at = now(),
		next_at = CASE WHEN $2 THEN next_at ELSE now() END
		WHERE id::text = $1 RETURNING name`, id, paused).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoDevice
	}
	return name, err
}

// CheckLabel is a check's name and kind, for the audit log.
func (s *Store) CheckLabel(ctx context.Context, id string) (name, kind string, err error) {
	err = s.db(ctx).QueryRow(ctx, `SELECT name, kind FROM checks WHERE id::text = $1`, id).Scan(&name, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNoWebsite
	}
	return name, kind, err
}

// checkTarget is how the audit log names a check: "website Shop",
// "network device core-sw1".
func checkTarget(name, kind string) string {
	if kind == "snmp" {
		return "network device " + name
	}
	return "website " + name
}

/* ── A round ─────────────────────────────────────────────────────────────── */

// deviceOpinion is one server's read of the device.
type deviceOpinion struct {
	host string
	res  tunnel.AttemptResult
	err  string // the server could not be asked
}

// readDevice asks every server at once.
func (a *API) readDevice(ctx context.Context, d NetworkDevice, q tunnel.SNMPQuery) []deviceOpinion {
	out := make([]deviceOpinion, len(d.CheckFrom))
	var wg sync.WaitGroup
	for i, id := range d.CheckFrom {
		wg.Add(1)
		go func() {
			defer wg.Done()
			asset, err := a.store.AssetByID(ctx, id)
			if err != nil {
				out[i] = deviceOpinion{host: "a removed server", err: "it is no longer in Argus"}
				return
			}
			out[i] = a.attemptFromServer(ctx, asset.Hostname, tunnel.GrantCheck{Kind: tunnel.CheckKindSNMP, SNMP: &q})
		}()
	}
	wg.Wait()
	return out
}

// attemptFromServer has the agent on hostname run one attempt.
func (a *API) attemptFromServer(ctx context.Context, hostname string, g tunnel.GrantCheck) deviceOpinion {
	if a.deviceAsk != nil {
		return a.deviceAsk(ctx, hostname, g)
	}
	op := deviceOpinion{host: hostname}
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		op.err = "this control plane cannot reach agents"
		return op
	}
	agentHost, err := a.agentOf(ctx, hostname)
	if err != nil {
		op.err = "its agent is not known"
		return op
	}
	gr := tunnel.NewGrant("argus-control", agentHost, "checkrun", "")
	gr.Service = tunnel.CheckRunService
	gr.Check = &g
	tok, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), gr)
	if err != nil {
		op.err = err.Error()
		return op
	}
	// The interface table is several requests after the first; discovery
	// instead tries each community string in turn, so budget for all of them.
	budget := tunnel.SNMPTimeoutDefault*3 + 15
	switch {
	case g.Discover != nil:
		per := g.Discover.TimeoutSec
		if per <= 0 {
			per = tunnel.SNMPDiscoverPerTry
		}
		budget = per*len(g.Discover.Communities) + 15
	case g.SNMP != nil && g.SNMP.TimeoutSec > 0:
		budget = g.SNMP.TimeoutSec*3 + 15
	}
	octx, cancel := context.WithTimeout(ctx, time.Duration(budget)*time.Second)
	defer cancel()
	st, open, err := a.Tunnels.OpenWith(octx, agentHost, tunnel.Header{Service: tunnel.CheckRunService, Grant: tok})
	switch {
	case errors.Is(err, ErrNoTunnel):
		op.err = "its agent is not connected"
		return op
	case tunnel.IsRefused(err) && (strings.Contains(open.Error, "unknown service") || strings.Contains(open.Error, tunnel.ErrUnknownCheckKind.Error())):
		op.err = "its agent needs updating to check network devices; update it from its Settings tab"
		return op
	case tunnel.IsRefused(err):
		op.err = open.Error
		return op
	case err != nil:
		op.err = "could not reach its agent"
		return op
	}
	defer st.Close()
	go func() {
		<-octx.Done()
		_ = st.Close()
	}()
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			op.err = "its agent went away before it answered"
			return op
		}
		if typ != tunnel.FrameAttemptResult {
			continue
		}
		if err := json.Unmarshal(p, &op.res); err != nil {
			op.err = "its agent's answer could not be read"
		}
		return op
	}
}

// deviceRound reads the device once from its servers and says what that
// means, with the answer that decided it.
func (a *API) deviceRound(ctx context.Context, d NetworkDevice) (websiteRoundResult, *tunnel.SNMPAnswer) {
	r := websiteRoundResult{At: time.Now().UTC()}
	q, err := a.deviceQuery(ctx, d)
	if err != nil {
		r.Outcome, r.Detail = RoundUnknown, "Argus could not check: "+err.Error()
		return r, nil
	}
	ops := a.readDevice(ctx, d, q)
	var notes, failedFrom []string
	var first *deviceOpinion
	for i := range ops {
		op := &ops[i]
		switch {
		case op.err != "":
			notes = append(notes, op.host+" could not be asked: "+op.err)
		case op.res.OK:
			ms := int(op.res.MS)
			r.MS = &ms
			r.Outcome, r.Detail = RoundUp, op.res.Detail+" (read from "+op.host+")"
			if first != nil && first.res.Fail == tunnel.SNMPFailNoAnswer {
				r.Outcome = RoundPartial
				r.Detail = fmt.Sprintf("%s could not reach it (%s), but %s could: %s", first.host, first.res.Detail, op.host, op.res.Detail)
			}
			return r, op.res.SNMP
		default:
			failedFrom = append(failedFrom, op.host)
			if first == nil {
				first = op
			}
		}
	}
	if first == nil {
		r.Outcome = RoundUnknown
		r.Detail = "Argus could not check: " + strings.Join(notes, "; ")
		return r, nil
	}
	ms := int(first.res.MS)
	r.MS = &ms
	r.Outcome = "fail"
	r.Confirmed = strings.Join(failedFrom, ", ")
	r.Detail = fmt.Sprintf("%s (from %s)", first.res.Detail, r.Confirmed)
	if len(notes) > 0 {
		r.Detail += ". " + strings.Join(notes, "; ")
	}
	// A device that answered with a down interface still said who it is.
	return r, first.res.SNMP
}

// nextDeviceInfo is what to keep from answer, given what was kept before:
// traffic rates from the counters' change, and a restart from the uptime
// going back.
func nextDeviceInfo(prev *DeviceInfo, ans *tunnel.SNMPAnswer, at time.Time, watch []WatchedInterface) (DeviceInfo, bool) {
	info := DeviceInfo{Name: ans.Name, Description: clip(ans.Description, 500), Location: clip(ans.Location, 200),
		UptimeSec: ans.UptimeSec, At: at, Interfaces: []DeviceInterface{}}
	restarted := false
	byIndex := map[int]DeviceInterface{}
	var dt float64
	if prev != nil {
		info.RestartedAt = prev.RestartedAt
		dt = at.Sub(prev.At).Seconds()
		// Uptime going back more than the time between answers can explain
		// is a restart; the 32-bit counter wrapping after 497 days is not.
		if prev.UptimeSec > 0 && ans.UptimeSec+60 < prev.UptimeSec && prev.UptimeSec < 4_294_967_295/100-86_400 {
			restarted = true
			t := at.Add(-time.Duration(ans.UptimeSec) * time.Second)
			info.RestartedAt = &t
		}
		for _, i := range prev.Interfaces {
			byIndex[i.Index] = i
		}
	}
	watched := map[int]bool{}
	for _, w := range watch {
		watched[w.Index] = true
	}
	rate := func(now, before uint64) *float64 {
		if dt <= 0 || dt > 3600 || now < before || restarted {
			return nil
		}
		v := float64(now-before) * 8 / dt
		return &v
	}
	for _, i := range ans.Interfaces {
		di := DeviceInterface{SNMPInterface: i, Watched: watched[i.Index]}
		if p, ok := byIndex[i.Index]; ok {
			di.InBps, di.OutBps = rate(i.InOctets, p.InOctets), rate(i.OutOctets, p.OutOctets)
			if now, before := i.InErrors+i.OutErrors, p.InErrors+p.OutErrors; now > before && !restarted {
				di.ErrorsUp = now - before
			}
		}
		info.Interfaces = append(info.Interfaces, di)
	}
	return info, restarted
}

// runDeviceRound reads d once and records what it found.
func (a *API) runDeviceRound(ctx context.Context, d NetworkDevice) {
	r, ans := a.deviceRound(ctx, d)
	if ctx.Err() != nil {
		_ = a.store.ReleaseWebsiteLease(context.WithoutCancel(ctx), d.ID)
		return
	}
	a.recordDeviceRound(ctx, d, r, ans)
}

// recordDeviceRound records a round and tells whoever should know.
func (a *API) recordDeviceRound(ctx context.Context, d NetworkDevice, r websiteRoundResult, ans *tunnel.SNMPAnswer) {
	var last []byte
	restarted := false
	if ans != nil {
		info, re := nextDeviceInfo(d.Device, ans, r.At, d.Watch)
		restarted = re
		last, _ = json.Marshal(info)
	}
	tr, err := a.store.recordCheckRound(ctx, d.ID, r, last)
	if err != nil {
		a.log.Error("network devices: cannot record a round", "device", d.Name, "error", err)
		return
	}
	if tr != nil {
		a.announceDevice(ctx, d, *tr)
	}
	if restarted {
		a.announceDeviceRestart(ctx, d, ans.UptimeSec)
	}
}

// startDueNetworkDevices leases what is due in ctx's organization and runs
// each round on its own goroutine, sharing the website checks' room.
func (a *API) startDueNetworkDevices(ctx context.Context) {
	free := cap(a.websites.sem) - len(a.websites.sem)
	if free <= 0 {
		return
	}
	list, err := a.store.TakeDueNetworkDevices(ctx, free)
	if err != nil {
		a.log.Error("network devices: cannot take due devices", "error", err)
		return
	}
	for _, d := range list {
		select {
		case a.websites.sem <- struct{}{}:
		default:
			_ = a.store.ReleaseWebsiteLease(ctx, d.ID)
			continue
		}
		a.websites.wg.Add(1)
		go func(d NetworkDevice) {
			defer a.websites.wg.Done()
			defer func() { <-a.websites.sem }()
			a.runDeviceRound(ctx, d)
		}(d)
	}
}

/* ── Telling people ──────────────────────────────────────────────────────── */

func deviceSubject(id string) string { return "device:" + id }

func deviceLink(id string) string { return "/network-devices?device=" + id }

// announceDevice tells admins, and whoever was given it, that a device went
// down or came back, in the bell and on the Websites' Telegram chats.
func (a *API) announceDevice(ctx context.Context, d NetworkDevice, tr checkTransition) {
	name := d.Name
	if org := orgLabel(ctx); org != "" {
		name += " (" + org + ")"
	}
	var title, detail, sev string
	switch {
	case tr.WentDown:
		sev = "critical"
		title = name + " is down"
		detail = fmt.Sprintf("%s has failed since %s UTC: %s. Confirmed over two rounds.",
			d.Host, tr.Outage.StartedAt.UTC().Format("2 Jan 15:04"), tr.Outage.Cause)
	case tr.CameBack:
		sev = "info"
		mins := int(tr.Recovered.Sub(tr.Outage.StartedAt).Round(time.Minute) / time.Minute)
		title = fmt.Sprintf("%s is back up after %d minute%s", name, mins, plural(mins))
		detail = fmt.Sprintf("%s answered again at %s UTC. It was down from %s UTC.", d.Host,
			tr.Recovered.UTC().Format("15:04"), tr.Outage.StartedAt.UTC().Format("2 Jan 15:04"))
		a.closeAlerts(ctx, deviceSubject(d.ID), "Back up at "+tr.Recovered.UTC().Format("15:04")+" UTC")
	default:
		return
	}
	al := ConsoleAlert{Kind: AlertWebsite, Severity: sev, Audience: AudienceAdmins, Title: title, Detail: detail, Link: deviceLink(d.ID)}
	if tr.WentDown {
		al.Subject = deviceSubject(d.ID)
	}
	a.raiseAlert(ctx, al)
	a.alertCheckWatchers(ctx, d.ID, al)
	icon := "🔴"
	if tr.CameBack {
		icon = "🟢"
	}
	a.telegramWebsite(ctx, WebsiteCheck{ID: d.ID, Name: d.Name, Telegram: TelegramDefault}, icon, title, detail, deviceLink(d.ID))
	a.log.Warn("network device "+map[bool]string{true: "down", false: "back up"}[tr.WentDown], "device", d.Name)
}

// announceDeviceRestart tells the bell a device restarted. Not an outage:
// it answered, and is told so someone can look at why.
func (a *API) announceDeviceRestart(ctx context.Context, d NetworkDevice, uptime int64) {
	mins := uptime / 60
	al := ConsoleAlert{Kind: AlertWebsite, Severity: "warning", Audience: AudienceAdmins,
		Title:  d.Name + " restarted",
		Detail: fmt.Sprintf("%s has been up for %d minute%s, so it restarted since the last check. It is answering.", d.Host, mins, plural(int(mins))),
		Link:   deviceLink(d.ID)}
	a.raiseAlert(ctx, al)
	a.alertCheckWatchers(ctx, d.ID, al)
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

// limitDevice leaves out how a device is read, for someone outside the
// fleet roles.
func limitDevice(d *NetworkDevice) {
	d.Limited = true
	d.User, d.AuthProto, d.PrivProto = "", "", ""
	d.CheckFrom, d.CheckHosts = []string{}, []string{}
	d.CreatedBy = ""
	d.HasSecret = false
}

func (a *API) deviceErr(w http.ResponseWriter, what string, err error) {
	var invalid ErrInvalidWebsite
	switch {
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, invalid.Reason)
	case errors.Is(err, ErrNoDevice):
		writeErr(w, http.StatusNotFound, "no such network device")
	default:
		a.fail(w, what, err)
	}
}

// decorateDevices fills in uptime, open outages and the servers' names.
func (a *API) decorateDevices(r *http.Request, list []NetworkDevice) error {
	up, err := a.store.WebsiteUptimes(r.Context(), time.Now())
	if err != nil {
		return err
	}
	open, err := a.store.WebsiteOutages(r.Context(), "", 1000)
	if err != nil {
		return err
	}
	byCheck := map[string]WebsiteOutage{}
	for _, o := range open {
		byCheck[o.CheckID] = o
	}
	names := map[string]string{}
	for i := range list {
		d := &list[i]
		d.Uptime = up[d.ID]
		if o, ok := byCheck[d.ID]; ok {
			d.OpenOutage = &o
		}
		for _, id := range d.CheckFrom {
			n, ok := names[id]
			if !ok {
				if as, err := a.store.AssetByID(r.Context(), id); err == nil {
					n = as.Hostname
					if as.Alias != "" {
						n = as.Alias
					}
				} else {
					n = "a removed server"
				}
				names[id] = n
			}
			d.CheckHosts = append(d.CheckHosts, n)
		}
	}
	return nil
}

func devicesShownTo(list []NetworkDevice, seen map[string]bool, role string) []NetworkDevice {
	out := list[:0]
	for _, d := range list {
		if seen == nil {
			d.CanOperate = role == "admin" || role == "owner"
			out = append(out, d)
			continue
		}
		op, ok := seen[d.ID]
		if !ok {
			continue
		}
		d.CanOperate = op
		limitDevice(&d)
		out = append(out, d)
	}
	return out
}

func (a *API) getNetworkDevices(w http.ResponseWriter, r *http.Request) {
	seen, role, ok := a.websiteReader(w, r)
	if !ok {
		return
	}
	list, err := a.store.NetworkDevices(r.Context())
	if err == nil {
		err = a.decorateDevices(r, list)
	}
	if err != nil {
		a.fail(w, "network devices", err)
		return
	}
	writeJSON(w, http.StatusOK, devicesShownTo(list, seen, role))
}

func (a *API) getNetworkDevice(w http.ResponseWriter, r *http.Request) {
	seen, role, ok := a.websiteReader(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, given := seen[id]; seen != nil && !given {
		writeErr(w, http.StatusNotFound, "no such network device")
		return
	}
	d, err := a.store.NetworkDevice(r.Context(), id)
	if err != nil {
		a.deviceErr(w, "network device", err)
		return
	}
	list := []NetworkDevice{d}
	if err := a.decorateDevices(r, list); err != nil {
		a.fail(w, "network device", err)
		return
	}
	rounds, err := a.store.WebsiteRounds(r.Context(), id, time.Now().Add(-24*time.Hour))
	if err != nil {
		a.fail(w, "network device rounds", err)
		return
	}
	outages, err := a.store.WebsiteOutages(r.Context(), id, 50)
	if err != nil {
		a.fail(w, "network device outages", err)
		return
	}
	list = devicesShownTo(list, seen, role)
	writeJSON(w, http.StatusOK, map[string]any{"device": list[0], "rounds": rounds, "outages": outages})
}

// deviceSummary is a device's settings in words, for the audit log. Never
// a credential.
func deviceSummary(d NetworkDevice) string {
	how := "SNMP v2c"
	if d.Version != tunnel.SNMPv2c {
		how = "SNMPv3 as " + d.User
	}
	var watch []string
	for _, w := range d.Watch {
		watch = append(watch, w.Name)
	}
	s := fmt.Sprintf("%s (%s:%d, %s, every %d min, read from %s", d.Name, d.Host, d.Port, how, d.EverySeconds/60, strings.Join(d.CheckHosts, ", "))
	if len(watch) > 0 {
		s += ", watching " + strings.Join(watch, ", ")
	}
	if d.Paused {
		s += ", paused"
	}
	return s + ")"
}

func (a *API) auditDevice(r *http.Request, action, severity, actor string, d NetworkDevice, detail string) {
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{Action: action, Severity: severity, ActorEmail: actor,
		Target: "network device " + d.Name, Detail: detail}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", action)
	}
}

func (a *API) postNetworkDevice(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	var in NetworkDeviceInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.prepareDevice(r.Context(), &in, nil); err != nil {
		a.deviceErr(w, "add network device", err)
		return
	}
	d, err := a.store.CreateNetworkDevice(r.Context(), in, actor)
	if err != nil {
		a.deviceErr(w, "add network device", err)
		return
	}
	list := []NetworkDevice{d}
	_ = a.decorateDevices(r, list)
	list[0].CanOperate = true
	a.auditDevice(r, "device.add", "notice", actor, list[0], "Added a network device: "+deviceSummary(list[0])+".")
	writeJSON(w, http.StatusCreated, list[0])
}

func (a *API) putNetworkDevice(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	var in NetworkDeviceInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := a.store.NetworkDevice(r.Context(), r.PathValue("id"))
	if err != nil {
		a.deviceErr(w, "change network device", err)
		return
	}
	if err := a.prepareDevice(r.Context(), &in, &saved); err != nil {
		a.deviceErr(w, "change network device", err)
		return
	}
	before, after, err := a.store.UpdateNetworkDevice(r.Context(), saved.ID, in)
	if err != nil {
		a.deviceErr(w, "change network device", err)
		return
	}
	list := []NetworkDevice{before, after}
	_ = a.decorateDevices(r, list)
	detail := "Changed a network device from " + deviceSummary(list[0]) + " to " + deviceSummary(list[1]) + "."
	if !slices.Equal(before.secretsSealed, after.secretsSealed) && (in.Community != "" || in.AuthPass != "" || in.PrivPass != "") {
		detail += " Its credentials were saved again."
	}
	a.auditDevice(r, "device.change", "notice", actor, list[1], detail)
	list[1].CanOperate = true
	writeJSON(w, http.StatusOK, list[1])
}

func (a *API) deleteNetworkDevice(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	d, err := a.store.DeleteNetworkDevice(r.Context(), r.PathValue("id"))
	if err != nil {
		a.deviceErr(w, "remove network device", err)
		return
	}
	a.closeAlerts(r.Context(), deviceSubject(d.ID), "The device was removed by "+actor)
	a.auditDevice(r, "device.remove", "warn", actor, d, "Removed the network device "+d.Host+" and its history.")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) postNetworkDeviceCheckNow(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.operatesCheck(w, r, sess.Role, sess.Email, r.PathValue("id")) {
		return
	}
	if err := a.store.CheckNow(r.Context(), r.PathValue("id")); err != nil {
		a.deviceErr(w, "check network device now", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// postNetworkDeviceTest reads a device once through the first server
// chosen, for the form's Test button: who it says it is and its interfaces,
// to choose which to watch. It saves nothing. ?id= names the device being
// edited, whose saved credentials an empty field keeps.
func (a *API) postNetworkDeviceTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.websiteAdmin(w, r); !ok {
		return
	}
	var in NetworkDeviceInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var before *NetworkDevice
	if id := r.URL.Query().Get("id"); id != "" {
		d, err := a.store.NetworkDevice(r.Context(), id)
		if err != nil {
			a.deviceErr(w, "test network device", err)
			return
		}
		before = &d
	}
	if err := a.prepareDevice(r.Context(), &in, before); err != nil {
		a.deviceErr(w, "test network device", err)
		return
	}
	asset, err := a.store.AssetByID(r.Context(), in.CheckFrom[0])
	if err != nil {
		writeErr(w, http.StatusBadRequest, "the server chosen to read it from is no longer in Argus")
		return
	}
	q := in.query()
	op := a.attemptFromServer(r.Context(), asset.Hostname, tunnel.GrantCheck{Kind: tunnel.CheckKindSNMP, SNMP: &q})
	if op.err != "" {
		writeJSON(w, http.StatusOK, tunnel.AttemptResult{Kind: tunnel.CheckKindSNMP, Fail: "server", Detail: op.host + " could not be asked: " + op.err})
		return
	}
	op.res.From = op.host
	writeJSON(w, http.StatusOK, op.res)
}

// snmpDefaultCommunities are the strings tried for every discovery: the
// read-only defaults a great deal of gear ships with.
var snmpDefaultCommunities = []string{"public", "private"}

// mergeCommunities is the defaults followed by the admin's own additions,
// trimmed, de-duplicated, and capped.
func mergeCommunities(extra []string) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(c string) {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			return
		}
		seen[c] = true
		out = append(out, c)
	}
	for _, c := range snmpDefaultCommunities {
		add(c)
	}
	for _, c := range extra {
		if len(out) >= 20 {
			break
		}
		add(c)
	}
	return out
}

// postNetworkDeviceDiscover tries to find a v2c community string for a
// device the admin owns, by trying the common defaults plus any strings
// they add, through the first server chosen, read-only. It saves nothing
// and never echoes the strings tried back.
func (a *API) postNetworkDeviceDiscover(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Host        string   `json:"host"`
		Port        int      `json:"port"`
		CheckFrom   []string `json:"checkFrom"`
		Communities []string `json:"communities"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	host := strings.TrimSpace(in.Host)
	port := in.Port
	if port == 0 {
		port = 161
	}
	if len(in.CheckFrom) == 0 || strings.TrimSpace(in.CheckFrom[0]) == "" {
		writeErr(w, http.StatusBadRequest, "choose a server on the device's network to read it from")
		return
	}
	d := tunnel.SNMPDiscover{Host: host, Port: port, TimeoutSec: tunnel.SNMPDiscoverPerTry, Communities: mergeCommunities(in.Communities)}
	if err := d.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	asset, err := a.store.AssetByID(r.Context(), strings.TrimSpace(in.CheckFrom[0]))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "the server chosen to read it from is no longer in Argus")
		return
	}
	op := a.attemptFromServer(r.Context(), asset.Hostname, tunnel.GrantCheck{Kind: tunnel.CheckKindSNMPDiscover, Discover: &d})
	if op.err != "" {
		writeJSON(w, http.StatusOK, tunnel.SNMPDiscovery{Detail: op.host + " could not be asked: " + op.err})
		return
	}
	res := tunnel.SNMPDiscovery{Tried: len(d.Communities)}
	if op.res.Discovery != nil {
		res = *op.res.Discovery
	}
	// Record that a discovery ran, never the strings themselves.
	found := "none found"
	if res.Found {
		found = "found one"
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{Action: "network_device.discover", Severity: "info",
		ActorEmail: actor, Target: "network device " + host,
		Detail: fmt.Sprintf("tried %d community strings via %s; %s", res.Tried, asset.Hostname, found)}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", "network_device.discover")
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) postNetworkDevicePause(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if !a.operatesCheck(w, r, sess.Role, sess.Email, id) {
		return
	}
	var in struct {
		Paused bool `json:"paused"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	d, err := a.store.NetworkDevice(r.Context(), id)
	if err != nil {
		a.deviceErr(w, "pause network device", err)
		return
	}
	if _, err := a.store.SetCheckPaused(r.Context(), id, in.Paused); err != nil {
		a.deviceErr(w, "pause network device", err)
		return
	}
	verb := "Resumed"
	if in.Paused {
		verb = "Paused"
	}
	a.auditDevice(r, "device.pause", "notice", sess.Email, d, verb+" checking the network device "+d.Host+".")
	w.WriteHeader(http.StatusNoContent)
}
