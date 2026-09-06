package control

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gsoultan/argus/internal/ratelimit"
)

// Throttles guards the authentication surface.
//
// Split by what an attempt costs rather than applying one number everywhere. A
// login redirect is cheap; a callback burns an IdP round trip; a failed
// authentication is the thing worth counting most tightly, because a client
// producing failures at a rate a legitimate user never reaches is doing
// something a legitimate user never does.
type Throttles struct {
	// login covers starting the OIDC flow and the callback that completes it.
	login *ratelimit.Limiter
	// failures counts refused authentications, far more tightly.
	failures *ratelimit.Limiter
	// tickets covers terminal authorisation, which is authenticated but still
	// worth bounding: a stolen session should not be able to mint tickets at
	// machine speed.
	tickets *ratelimit.Limiter

	resolver *ratelimit.ClientResolver
	stop     chan struct{}
}

// ThrottleConfig tunes the limits.
type ThrottleConfig struct {
	// TrustedProxies are networks whose forwarding headers are believed. Empty
	// means headers are ignored, which is correct when reached directly — a
	// header the client sets would otherwise let them both evade their own
	// limit and throttle somebody else.
	TrustedProxies []string

	LoginPerMinute   int
	FailuresPerHour  int
	TicketsPerMinute int
}

// NewThrottles builds the limiters.
func NewThrottles(cfg ThrottleConfig) (*Throttles, error) {
	resolver, err := ratelimit.NewClientResolver(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}

	// Defaults chosen so a person never notices and a script always does.
	if cfg.LoginPerMinute <= 0 {
		cfg.LoginPerMinute = 20
	}
	if cfg.FailuresPerHour <= 0 {
		cfg.FailuresPerHour = 20
	}
	if cfg.TicketsPerMinute <= 0 {
		cfg.TicketsPerMinute = 30
	}

	t := &Throttles{
		resolver: resolver,
		stop:     make(chan struct{}),
		login: ratelimit.New(ratelimit.Limit{
			Rate: cfg.LoginPerMinute, Window: time.Minute,
			// A browser can legitimately fire several requests at once.
			Burst: cfg.LoginPerMinute / 2,
		}, ratelimit.DefaultMaxKeys),
		failures: ratelimit.New(ratelimit.Limit{
			Rate: cfg.FailuresPerHour, Window: time.Hour,
			// Small: a person mistypes a few times, not fifty. Never more than
			// the configured rate, or an operator who set failures_per_hour: 3
			// would find five getting through before anything was refused.
			Burst: min(5, cfg.FailuresPerHour),
		}, ratelimit.DefaultMaxKeys),
		tickets: ratelimit.New(ratelimit.Limit{
			Rate: cfg.TicketsPerMinute, Window: time.Minute,
			Burst: cfg.TicketsPerMinute,
		}, ratelimit.DefaultMaxKeys),
	}

	for _, l := range []*ratelimit.Limiter{t.login, t.failures, t.tickets} {
		l.StartSweeper(5*time.Minute, t.stop)
	}
	return t, nil
}

// Close stops the background sweepers.
func (t *Throttles) Close() {
	if t != nil && t.stop != nil {
		close(t.stop)
	}
}

// limitLogin wraps an endpoint that begins or completes authentication.
func (a *API) limitLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.throttles == nil {
			next(w, r)
			return
		}
		key := a.throttles.resolver.Key(r)

		// A client already over the failure budget is refused before the work
		// is done, so a brute force costs them a request and us nothing.
		//
		// A peek, not a spend. Only RecordAuthFailure charges this budget --
		// checking it here with Allow and handing the token back with Reset
		// refilled the whole bucket on every attempt, which meant the budget
		// could never be exceeded and the limit never refused anyone.
		if exhausted, retry := a.throttles.failures.Exhausted(key); exhausted {
			a.refuse(w, r, key, retry, "too many failed authentication attempts")
			return
		}

		if ok, retry := a.throttles.login.Allow(key); !ok {
			a.refuse(w, r, key, retry, "too many authentication attempts")
			return
		}
		next(w, r)
	}
}

// limitTickets wraps terminal authorisation.
func (a *API) limitTickets(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.throttles == nil {
			next(w, r)
			return
		}
		key := a.throttles.resolver.Key(r)
		if ok, retry := a.throttles.tickets.Allow(key); !ok {
			a.refuse(w, r, key, retry, "too many terminal requests")
			return
		}
		next(w, r)
	}
}

// RecordAuthFailure charges a failed authentication against the client.
//
// Called only on genuine failures, so someone signing in normally never touches
// this budget however often they do it.
func (a *API) RecordAuthFailure(r *http.Request) {
	if a.throttles == nil {
		return
	}
	key := a.throttles.resolver.Key(r)
	if ok, _ := a.throttles.failures.Allow(key); !ok {
		a.log.Warn("client exceeded the failed-authentication budget",
			"client", key,
			"detail", "further attempts will be refused until the window elapses")
	}
}

// RecordAuthSuccess clears the failure budget, so a user who mistyped once is
// not throttled for the rest of the hour.
func (a *API) RecordAuthSuccess(r *http.Request) {
	if a.throttles == nil {
		return
	}
	a.throttles.failures.Reset(a.throttles.resolver.Key(r))
}

func (a *API) refuse(w http.ResponseWriter, r *http.Request, key string,
	retry time.Duration, reason string) {

	seconds := int(retry.Seconds()) + 1
	a.log.Warn("request throttled",
		"client", key, "path", r.URL.Path, "retry_after_seconds", seconds)

	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	// Deliberately says nothing about whether the credential was valid — a
	// throttle response should not become an oracle.
	writeErr(w, http.StatusTooManyRequests, reason)
}
