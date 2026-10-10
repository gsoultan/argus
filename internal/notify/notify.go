// Package notify tells administrators, out of band, that someone opened a
// session.
//
// Every session is recorded and lands in the console, but a console is
// something you have to go and look at. An administrator who wants to know the
// moment somebody is on a production host -- at three in the morning, or when
// the somebody went around the gateway -- needs it pushed to them: an email, or
// a Telegram message on their phone.
//
// Delivery never sits in the path of the session itself. A report is accepted,
// stored and answered first; the alert is queued and sent by a worker. A slow
// or unreachable mail server must not stall the reporter that told us about the
// session, and must never be the reason a session report was lost.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// SessionStarted is what an alert says about a new session.
type SessionStarted struct {
	ID        string
	UserEmail string
	Host      string
	Principal string
	Protocol  string
	// Origin is "brokered" for a session opened through Argus, and "direct" for
	// one the host agent saw arrive without it.
	Origin    string
	ClientIP  string
	StartedAt time.Time
}

// Sender delivers one message to one channel.
type Sender interface {
	// Name identifies the channel in logs: "email" or "telegram".
	Name() string
	Send(ctx context.Context, subject, body string) error
}

// Message renders an alert as a subject and a plain-text body.
//
// Plain text because it is read on a lock screen as often as in a mail client,
// and the line that matters -- who, where, as whom -- has to survive being cut
// to the first sixty characters.
func Message(e SessionStarted, consoleURL string) (subject, body string) {
	who := e.UserEmail
	if who == "" {
		who = "someone"
	}
	as := e.Principal
	if as == "" {
		as = "an unknown account"
	}
	proto := strings.ToUpper(e.Protocol)
	if proto == "" {
		proto = "a session"
	}

	if e.Origin == "direct" {
		subject = fmt.Sprintf("Argus: %s logged in to %s as %s without going through Argus", who, e.Host, as)
	} else {
		subject = fmt.Sprintf("Argus: %s opened %s on %s as %s", who, proto, e.Host, as)
	}

	var b strings.Builder
	b.WriteString(subject[len("Argus: "):])
	b.WriteString(".\n\n")
	fmt.Fprintf(&b, "Server:  %s\n", e.Host)
	fmt.Fprintf(&b, "Account: %s\n", as)
	if e.UserEmail != "" {
		fmt.Fprintf(&b, "Person:  %s\n", e.UserEmail)
	}
	if e.ClientIP != "" {
		fmt.Fprintf(&b, "From:    %s\n", e.ClientIP)
	}
	if !e.StartedAt.IsZero() {
		fmt.Fprintf(&b, "Started: %s\n", e.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	if e.Origin == "direct" {
		b.WriteString("\nThis login did not go through Argus. The host agent recorded it, " +
			"but nobody approved it through Argus.\n")
	}
	if consoleURL != "" && e.ID != "" {
		fmt.Fprintf(&b, "\nWatch or replay it: %s/sessions/%s\n", strings.TrimRight(consoleURL, "/"), e.ID)
	}
	return subject, b.String()
}

// Dispatcher queues alerts and sends them from a single worker.
type Dispatcher struct {
	senders    []Sender
	consoleURL string
	log        *slog.Logger
	timeout    time.Duration

	queue chan SessionStarted
	done  chan struct{}
	once  sync.Once
}

// queueSize bounds how far alerts can back up behind a dead mail server.
//
// Past it new alerts are dropped and logged, rather than held in memory without
// limit: a burst of sessions during an outage of the alert channel must not
// become an outage of the control plane.
const queueSize = 256

// NewDispatcher starts the worker. With no senders it returns nil, and a nil
// Dispatcher accepts and discards everything, so callers need no special case
// for "alerts are not configured".
func NewDispatcher(senders []Sender, consoleURL string, log *slog.Logger) *Dispatcher {
	if len(senders) == 0 {
		return nil
	}
	d := &Dispatcher{
		senders:    senders,
		consoleURL: consoleURL,
		log:        log,
		timeout:    15 * time.Second,
		queue:      make(chan SessionStarted, queueSize),
		done:       make(chan struct{}),
	}
	go d.run()
	return d
}

// SessionStarted queues an alert. It never blocks.
func (d *Dispatcher) SessionStarted(e SessionStarted) {
	if d == nil {
		return
	}
	select {
	case d.queue <- e:
	default:
		d.log.Warn("alert queue full; dropping a session alert",
			"session", e.ID, "host", e.Host, "user", e.UserEmail,
			"detail", "the alert channels are not keeping up; the session itself is recorded")
	}
}

// Close stops accepting alerts and waits for the queued ones to be sent, up to
// ctx's deadline.
func (d *Dispatcher) Close(ctx context.Context) {
	if d == nil {
		return
	}
	d.once.Do(func() { close(d.queue) })
	select {
	case <-d.done:
	case <-ctx.Done():
		d.log.Warn("shutting down with session alerts still unsent", "queued", len(d.queue))
	}
}

func (d *Dispatcher) run() {
	defer close(d.done)
	for e := range d.queue {
		subject, body := Message(e, d.consoleURL)
		for _, s := range d.senders {
			ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
			err := s.Send(ctx, subject, body)
			cancel()
			if err != nil {
				d.log.Error("session alert not delivered",
					"channel", s.Name(), "session", e.ID, "host", e.Host, "error", err)
				continue
			}
			d.log.Info("session alert sent", "channel", s.Name(), "session", e.ID, "host", e.Host)
		}
	}
}

// ErrNotConfigured is returned by a constructor given an incomplete channel.
var ErrNotConfigured = errors.New("alert channel is incomplete")
