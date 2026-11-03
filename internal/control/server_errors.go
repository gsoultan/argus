package control

import (
	"sync"
	"time"
	"unicode/utf8"
)

/*
The requests that failed on this process, kept for the Status page.

Only what a.fail is told, which is what the log line says: the operation and
the error. Never the request, its body or its credentials. In memory and
bounded, so a storm of failures costs a fixed amount and a restart clears it;
the page says "since it started" for that reason.
*/

const (
	// recentErrorsKept is how many distinct failures the page can list.
	recentErrorsKept = 50
	// errorMessageMax bounds what one failure holds.
	errorMessageMax = 300
)

// ServerError is one kind of failure: the same operation failing the same way
// is counted on one row rather than filling the list.
type ServerError struct {
	At      time.Time `json:"at"`
	Op      string    `json:"op"`
	Message string    `json:"message"`
	Count   int       `json:"count"`
}

// ErrorCounts is how many requests failed, and the latest kinds.
type ErrorCounts struct {
	LastHour   int           `json:"lastHour"`
	SinceStart int           `json:"sinceStart"`
	Recent     []ServerError `json:"recent"`
}

type errorLog struct {
	mu     sync.Mutex
	recent []ServerError // newest last
	total  int
	// minutes counts failures per minute for the last hour, as a ring keyed
	// by the minute's unix number, so the hour's count needs no timestamps.
	minutes [60]struct {
		minute int64
		n      int
	}
}

// clipMessage bounds msg to errorMessageMax bytes without splitting a rune.
func clipMessage(msg string) string {
	if len(msg) <= errorMessageMax {
		return msg
	}
	cut := errorMessageMax
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func (l *errorLog) record(now time.Time, op, msg string) {
	msg = clipMessage(msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	m := now.Unix() / 60
	b := &l.minutes[m%60]
	if b.minute != m {
		b.minute, b.n = m, 0
	}
	b.n++

	for i := range l.recent {
		if l.recent[i].Op == op && l.recent[i].Message == msg {
			e := l.recent[i]
			e.Count++
			e.At = now
			l.recent = append(append(l.recent[:i:i], l.recent[i+1:]...), e)
			return
		}
	}
	if len(l.recent) == recentErrorsKept {
		l.recent = append(l.recent[:0:0], l.recent[1:]...)
	}
	l.recent = append(l.recent, ServerError{At: now, Op: op, Message: msg, Count: 1})
}

func (l *errorLog) counts(now time.Time) ErrorCounts {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := ErrorCounts{SinceStart: l.total, Recent: make([]ServerError, 0, len(l.recent))}
	m := now.Unix() / 60
	for _, b := range l.minutes {
		if b.n > 0 && m-b.minute < 60 && b.minute <= m {
			c.LastHour += b.n
		}
	}
	for i := len(l.recent) - 1; i >= 0; i-- {
		c.Recent = append(c.Recent, l.recent[i])
	}
	return c
}

// uploadLog is how the last recordings shipped to this process fared.
type uploadLog struct {
	mu        sync.Mutex
	lastOK    time.Time
	lastFail  time.Time
	lastError string
}

func (u *uploadLog) ok(now time.Time) {
	u.mu.Lock()
	u.lastOK = now
	u.mu.Unlock()
}

func (u *uploadLog) failed(now time.Time, err error) {
	u.mu.Lock()
	u.lastFail, u.lastError = now, clipMessage(err.Error())
	u.mu.Unlock()
}

func (u *uploadLog) snapshot() (ok, fail time.Time, msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastOK, u.lastFail, u.lastError
}
