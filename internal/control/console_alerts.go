package control

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Console alerts: the bell in the console header.
//
// Email and Telegram tell people who are away from the console. This tells the
// people who are in it, the moment something wants their attention: an access
// request they can decide, someone signing in, a session opening, a security
// event. Each is a row in console_alerts and a nudge down every open stream
// (see console_alert_routes.go), so a console that was closed reads what it
// missed from the table and one that was open is told at once.
//
// Who may read a row is decided by its audience, by the same rule in SQL
// (alertVisibleSQL) and in memory (alertVisible) for what the stream pushes,
// and a test holds the two to each other. Nothing here is evidence: the audit
// chain records the event; an alert only points at it and is gone in 30 days.

// Alert kinds. A person can mute any of them except security, which is rare
// and is the reason to have a bell at all.
const (
	AlertApproval = "approval"
	AlertSignIn   = "signin"
	AlertSession  = "session"
	AlertSecurity = "security"
	AlertServer   = "server"
	AlertBackup   = "backup"
)

// AlertKinds is every kind, in the order Settings lists them.
var AlertKinds = []string{AlertApproval, AlertSignIn, AlertSession, AlertSecurity, AlertServer, AlertBackup}

// Alert audiences.
const (
	// AudienceDeciders is everyone who may decide an access request.
	AudienceDeciders = "deciders"
	// AudienceAdmins is everyone who reads the whole organization.
	AudienceAdmins = "admins"
	// AudiencePerson is the one account named in Recipient.
	AudiencePerson = "person"
)

// alertRetention is how long an alert is kept.
const alertRetention = 30 * 24 * time.Hour

// ConsoleAlert is one alert as the console shows it.
type ConsoleAlert struct {
	Seq      int64     `json:"seq"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Severity string    `json:"severity"`
	Title    string    `json:"title"`
	Detail   string    `json:"detail"`
	Link     string    `json:"link"`
	Read     bool      `json:"read"`

	// Who may see it; never sent to a browser.
	Audience  string `json:"-"`
	Recipient string `json:"-"`
	Actor     string `json:"-"`
}

// AlertPrefs is what one person chose for their bell.
type AlertPrefs struct {
	Muted          []string `json:"muted"`
	NewDevicesOnly bool     `json:"newDevicesOnly"`
	Popup          bool     `json:"popup"`
	Sound          bool     `json:"sound"`
}

// DefaultAlertPrefs is what a person who never opened Settings gets.
var DefaultAlertPrefs = AlertPrefs{Muted: []string{}, Popup: true}

// ErrUnknownAlertKind is a preference naming a kind this build does not have.
var ErrUnknownAlertKind = errors.New("unknown alert kind")

// alertReader is who is asking: the parts of a session visibility depends on.
type alertReader struct {
	email string
	role  string
	prefs AlertPrefs
}

func decidesRequests(role string) bool {
	return role == "approver" || role == "admin" || role == "owner"
}

// readsAdminAlerts is seesWholeFleet: whoever reads the audit log, where all
// of these are recorded anyway, is told of them.
func readsAdminAlerts(role string) bool { return seesWholeFleet(role) }

// alertVisible is the in-memory twin of alertVisibleSQL.
func alertVisible(rd alertReader, al ConsoleAlert) bool {
	email := strings.ToLower(rd.email)
	switch al.Audience {
	case AudiencePerson:
		if al.Recipient != email {
			return false
		}
	case AudienceDeciders:
		if !decidesRequests(rd.role) || al.Actor == email {
			return false
		}
	case AudienceAdmins:
		if !readsAdminAlerts(rd.role) || al.Actor == email {
			return false
		}
	default:
		return false
	}
	if al.Kind != AlertSecurity && slices.Contains(rd.prefs.Muted, al.Kind) {
		return false
	}
	if al.Kind == AlertSignIn && rd.prefs.NewDevicesOnly && al.Severity == "info" {
		return false
	}
	return true
}

// alertVisibleSQL is the WHERE clause for what rd may read, over columns of
// console_alerts aliased a, with its arguments starting at $1.
func alertVisibleSQL(rd alertReader) (string, []any) {
	muted := slices.DeleteFunc(slices.Clone(rd.prefs.Muted), func(k string) bool { return k == AlertSecurity })
	if muted == nil {
		muted = []string{}
	}
	return `((a.audience = 'person' AND a.recipient = $1)
	      OR (a.audience = 'deciders' AND $2 AND a.actor <> $1)
	      OR (a.audience = 'admins' AND $3 AND a.actor <> $1))
	  AND NOT (a.kind = ANY($4::text[]))
	  AND NOT (a.kind = 'signin' AND $5 AND a.severity = 'info')`,
		[]any{strings.ToLower(rd.email), decidesRequests(rd.role), readsAdminAlerts(rd.role), muted, rd.prefs.NewDevicesOnly}
}

/* ── Raising one ─────────────────────────────────────────────────────────── */

// raiseAlert stores al for the organization on ctx and tells every open
// console on this replica. It never fails its caller: the event it describes
// has already happened and is in the audit chain; a missing notice is logged.
func (a *API) raiseAlert(ctx context.Context, al ConsoleAlert) {
	al.Recipient = strings.ToLower(strings.TrimSpace(al.Recipient))
	al.Actor = strings.ToLower(strings.TrimSpace(al.Actor))
	al.Title = clip(al.Title, 300)
	al.Detail = clip(al.Detail, 1000)
	// A fresh deadline that outlives the request: the alert is for an event
	// that has happened whether or not this client is still waiting.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	out, err := a.store.InsertConsoleAlert(sctx, al)
	if err != nil {
		a.log.Error("could not store a console alert", "kind", al.Kind, "error", err)
		return
	}
	a.alertHub.publish(tenantIDOf(ctx), out)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}

/* ── Store ──────────────────────────────────────────────────────────────── */

const alertColumns = `a.seq, a.at, a.kind, a.severity, a.title, a.detail, a.link, a.audience, a.recipient, a.actor`

func scanAlert(row pgx.Row, extra ...any) (ConsoleAlert, error) {
	var al ConsoleAlert
	dest := append([]any{&al.Seq, &al.At, &al.Kind, &al.Severity, &al.Title, &al.Detail, &al.Link,
		&al.Audience, &al.Recipient, &al.Actor}, extra...)
	err := row.Scan(dest...)
	return al, err
}

// InsertConsoleAlert stores one alert and returns it with its seq and time.
func (s *Store) InsertConsoleAlert(ctx context.Context, al ConsoleAlert) (ConsoleAlert, error) {
	if al.Severity == "" {
		al.Severity = "info"
	}
	row := s.db(ctx).QueryRow(ctx, `
		INSERT INTO console_alerts AS a (kind, severity, audience, recipient, actor, title, detail, link)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING `+alertColumns,
		al.Kind, al.Severity, al.Audience, al.Recipient, al.Actor, al.Title, al.Detail, al.Link)
	return scanAlert(row)
}

// ConsoleAlertsSince is every alert after seq, oldest first, at most limit:
// what another replica raised, for this one's streams.
func (s *Store) ConsoleAlertsSince(ctx context.Context, seq int64, limit int) ([]ConsoleAlert, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+alertColumns+` FROM console_alerts a
		WHERE a.seq > $1 ORDER BY a.seq LIMIT $2`, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConsoleAlert
	for rows.Next() {
		al, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, al)
	}
	return out, rows.Err()
}

// LatestConsoleAlertSeq is the newest alert's seq, or 0.
func (s *Store) LatestConsoleAlertSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := s.db(ctx).QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM console_alerts`).Scan(&seq)
	return seq, err
}

// AlertPrefs is email's choices, or the defaults.
func (s *Store) AlertPrefs(ctx context.Context, email string) (AlertPrefs, error) {
	p := DefaultAlertPrefs
	err := s.db(ctx).QueryRow(ctx, `
		SELECT muted, new_devices_only, popup, sound FROM console_alert_prefs WHERE email = $1`,
		strings.ToLower(email)).Scan(&p.Muted, &p.NewDevicesOnly, &p.Popup, &p.Sound)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultAlertPrefs, nil
	}
	if p.Muted == nil {
		p.Muted = []string{}
	}
	return p, err
}

// SetAlertPrefs saves email's choices.
func (s *Store) SetAlertPrefs(ctx context.Context, email string, p AlertPrefs) (AlertPrefs, error) {
	muted := []string{}
	for _, k := range p.Muted {
		if !slices.Contains(AlertKinds, k) {
			return AlertPrefs{}, ErrUnknownAlertKind
		}
		if k != AlertSecurity && !slices.Contains(muted, k) {
			muted = append(muted, k)
		}
	}
	p.Muted = muted
	_, err := s.db(ctx).Exec(ctx, `
		INSERT INTO console_alert_prefs (email, muted, new_devices_only, popup, sound)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (email) DO UPDATE SET muted = $2, new_devices_only = $3, popup = $4, sound = $5,
			updated_at = now()`,
		strings.ToLower(email), p.Muted, p.NewDevicesOnly, p.Popup, p.Sound)
	return p, err
}

// AlertPage is one page of a person's alerts.
type AlertPage struct {
	Alerts []ConsoleAlert `json:"alerts"`
	Unread int            `json:"unread"`
	// More is whether older alerts remain past this page.
	More bool `json:"more"`
}

// ConsoleAlerts lists what rd may read, newest first, before seq when before
// is above zero; only the unread ones when unreadOnly.
func (s *Store) ConsoleAlerts(ctx context.Context, rd alertReader, unreadOnly bool, before int64, limit int) (AlertPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	where, args := alertVisibleSQL(rd)
	n := len(args)
	readExpr := `(a.seq <= coalesce(p.read_seq, 0) OR r.seq IS NOT NULL)`
	q := `SELECT ` + alertColumns + `, ` + readExpr + ` FROM console_alerts a
		LEFT JOIN console_alert_prefs p ON p.email = $1
		LEFT JOIN console_alert_reads r ON r.email = $1 AND r.seq = a.seq
		WHERE ` + where
	if unreadOnly {
		q += ` AND NOT ` + readExpr
	}
	if before > 0 {
		args = append(args, before)
		n++
		q += ` AND a.seq < $` + strconv.Itoa(n)
	}
	args = append(args, limit+1)
	n++
	q += ` ORDER BY a.seq DESC LIMIT $` + strconv.Itoa(n)

	db := s.db(ctx)
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return AlertPage{}, err
	}
	defer rows.Close()
	page := AlertPage{Alerts: []ConsoleAlert{}}
	for rows.Next() {
		var read bool
		al, err := scanAlert(rows, &read)
		if err != nil {
			return AlertPage{}, err
		}
		al.Read = read
		page.Alerts = append(page.Alerts, al)
	}
	if err := rows.Err(); err != nil {
		return AlertPage{}, err
	}
	if len(page.Alerts) > limit {
		page.Alerts, page.More = page.Alerts[:limit], true
	}
	page.Unread, err = s.UnreadConsoleAlerts(ctx, rd)
	return page, err
}

// unreadCap is where the count stops: the badge says 99+ past it, and
// counting further would only cost.
const unreadCap = 100

// UnreadConsoleAlerts counts what rd may read and has not, up to unreadCap.
func (s *Store) UnreadConsoleAlerts(ctx context.Context, rd alertReader) (int, error) {
	where, args := alertVisibleSQL(rd)
	var n int
	err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM console_alerts a
		LEFT JOIN console_alert_prefs p ON p.email = $1
		LEFT JOIN console_alert_reads r ON r.email = $1 AND r.seq = a.seq
		WHERE `+where+` AND a.seq > coalesce(p.read_seq, 0) AND r.seq IS NULL
		LIMIT `+strconv.Itoa(unreadCap)+`) x`, args...).Scan(&n)
	return n, err
}

// MarkConsoleAlertsRead marks seqs read for email, or everything up to and
// including upTo when it is above zero.
func (s *Store) MarkConsoleAlertsRead(ctx context.Context, email string, seqs []int64, upTo int64) error {
	email = strings.ToLower(email)
	return pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		if upTo > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO console_alert_prefs (email, read_seq) VALUES ($1, $2)
				ON CONFLICT (email) DO UPDATE SET read_seq = greatest(console_alert_prefs.read_seq, $2)`,
				email, upTo); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `DELETE FROM console_alert_reads WHERE email = $1 AND seq <= $2`, email, upTo)
			return err
		}
		if len(seqs) == 0 {
			return nil
		}
		// Only alerts that exist; one that was swept is simply not marked.
		_, err := tx.Exec(ctx, `
			INSERT INTO console_alert_reads (email, seq)
			SELECT $1, seq FROM console_alerts WHERE seq = ANY($2::bigint[])
			ON CONFLICT DO NOTHING`, email, seqs)
		return err
	})
}

// SweepConsoleAlerts deletes alerts past their retention.
func (s *Store) SweepConsoleAlerts(ctx context.Context) (int64, error) {
	tag, err := s.db(ctx).Exec(ctx, `DELETE FROM console_alerts WHERE at < now() - make_interval(secs => $1)`,
		alertRetention.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

/* ── Fan-out ─────────────────────────────────────────────────────────────── */

// Bounds on open streams. Each is a goroutine and a connection, and the key
// is an account a caller signs in as, so both are capped.
const (
	maxAlertStreams           = 2000
	maxAlertStreamsPerAccount = 8
	// alertPollEvery is how often a replica looks for alerts another replica
	// raised, while anyone here is listening.
	alertPollEvery = 3 * time.Second
)

// ErrTooManyAlertStreams is a stream refused by the bounds above.
var ErrTooManyAlertStreams = errors.New("too many open alert streams")

// alertEvent is what a stream is sent.
type alertEvent struct {
	// alert is set for a new alert.
	alert *ConsoleAlert
	// read is set when the subscriber's own read state or preferences changed
	// in another tab: the console refetches.
	read bool
}

type alertSub struct {
	email string
	ch    chan alertEvent
}

// alertHub is this replica's open streams, by organization.
type alertHub struct {
	mu      sync.Mutex
	tenants map[string]*tenantAlerts
	total   int
	closed  bool
	store   *Store
	logErr  func(msg string, args ...any)
}

type tenantAlerts struct {
	tenant *Tenant
	subs   map[*alertSub]struct{}
	// last is the newest seq this replica has sent; the poller sends what
	// is above it.
	last int64
	stop chan struct{}
}

// subscribe opens a stream for email in tenant t.
func (h *alertHub) subscribe(ctx context.Context, email string) (*alertSub, func(), error) {
	t := TenantOf(ctx)
	tid := tenantIDOf(ctx)
	email = strings.ToLower(email)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, errors.New("shutting down")
	}
	if h.total >= maxAlertStreams {
		return nil, nil, ErrTooManyAlertStreams
	}
	if h.tenants == nil {
		h.tenants = map[string]*tenantAlerts{}
	}
	ta := h.tenants[tid]
	if ta != nil {
		n := 0
		for s := range ta.subs {
			if s.email == email {
				n++
			}
		}
		if n >= maxAlertStreamsPerAccount {
			return nil, nil, ErrTooManyAlertStreams
		}
	}
	if ta == nil {
		ta = &tenantAlerts{tenant: t, subs: map[*alertSub]struct{}{}, stop: make(chan struct{})}
		h.tenants[tid] = ta
		if h.store != nil {
			go h.poll(tid, ta)
		}
	}
	s := &alertSub{email: email, ch: make(chan alertEvent, 32)}
	ta.subs[s] = struct{}{}
	h.total++
	return s, func() { h.unsubscribe(tid, s) }, nil
}

func (h *alertHub) unsubscribe(tid string, s *alertSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ta := h.tenants[tid]
	if ta == nil {
		return
	}
	if _, ok := ta.subs[s]; !ok {
		return
	}
	delete(ta.subs, s)
	close(s.ch)
	h.total--
	if len(ta.subs) == 0 {
		close(ta.stop)
		delete(h.tenants, tid)
	}
}

// publish sends a new alert to every stream in the organization. Never
// blocks: a stream too far behind is closed, and the console reconnects and
// refetches, which is a better outcome than holding up the request that
// raised the alert.
func (h *alertHub) publish(tid string, al ConsoleAlert) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ta := h.tenants[tid]
	if ta == nil {
		return
	}
	if al.Seq > ta.last {
		ta.last = al.Seq
	}
	h.sendLocked(tid, ta, func(*alertSub) (alertEvent, bool) { c := al; return alertEvent{alert: &c}, true })
}

// touched tells email's own streams that their read state or preferences
// changed, so every tab they have open agrees.
func (h *alertHub) touched(tid, email string) {
	email = strings.ToLower(email)
	h.mu.Lock()
	defer h.mu.Unlock()
	ta := h.tenants[tid]
	if ta == nil {
		return
	}
	h.sendLocked(tid, ta, func(s *alertSub) (alertEvent, bool) { return alertEvent{read: true}, s.email == email })
}

func (h *alertHub) sendLocked(tid string, ta *tenantAlerts, ev func(*alertSub) (alertEvent, bool)) {
	for s := range ta.subs {
		e, ok := ev(s)
		if !ok {
			continue
		}
		select {
		case s.ch <- e:
		default:
			delete(ta.subs, s)
			close(s.ch)
			h.total--
		}
	}
	if len(ta.subs) == 0 {
		close(ta.stop)
		delete(h.tenants, tid)
	}
}

// poll picks up alerts raised on other replicas while anyone here listens.
//
// One query per organization per interval, whatever the number of streams.
// Seqs can commit out of order across replicas, so an alert committed late
// with a seq below one already sent is not pushed; the console still finds
// it, because it refetches on every event and on reconnect.
func (h *alertHub) poll(tid string, ta *tenantAlerts) {
	ctx := WithTenant(context.Background(), ta.tenant)
	if seq, err := h.store.LatestConsoleAlertSeq(ctx); err == nil {
		h.mu.Lock()
		if seq > ta.last {
			ta.last = seq
		}
		h.mu.Unlock()
	}
	t := time.NewTicker(alertPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ta.stop:
			return
		case <-t.C:
		}
		h.mu.Lock()
		last := ta.last
		h.mu.Unlock()
		qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		news, err := h.store.ConsoleAlertsSince(qctx, last, 50)
		cancel()
		if err != nil {
			if h.logErr != nil {
				h.logErr("could not look for new console alerts", "error", err)
			}
			continue
		}
		for _, al := range news {
			h.mu.Lock()
			if h.tenants[tid] != ta {
				h.mu.Unlock()
				return
			}
			if al.Seq > ta.last {
				h.sendLockedAlert(tid, ta, al)
			}
			h.mu.Unlock()
		}
	}
}

func (h *alertHub) sendLockedAlert(tid string, ta *tenantAlerts, al ConsoleAlert) {
	ta.last = al.Seq
	h.sendLocked(tid, ta, func(*alertSub) (alertEvent, bool) { c := al; return alertEvent{alert: &c}, true })
}

// closeAll ends every stream, for a shutdown or a restart on new settings:
// http.Server.Shutdown waits for handlers, and a stream never returns on its own.
func (h *alertHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for tid, ta := range h.tenants {
		for s := range ta.subs {
			close(s.ch)
		}
		close(ta.stop)
		delete(h.tenants, tid)
	}
	h.total = 0
}

// StopAlertStreams ends every open alert stream; register it with
// http.Server.RegisterOnShutdown.
func (a *API) StopAlertStreams() { a.alertHub.closeAll() }
