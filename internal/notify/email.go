package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// EmailConfig is the `alerts.email` block of the control plane's config.
type EmailConfig struct {
	// Host and Port of the SMTP server. Port 465 is implicit TLS; any other
	// port must offer STARTTLS, unless the server is on this machine.
	Host string `yaml:"host,omitempty"`
	Port int    `yaml:"port,omitempty"`
	// Username and Password are optional; with neither, no AUTH is sent.
	// Put the password in the environment or a file: "${file:/path}".
	Username string   `yaml:"username,omitempty"`
	Password string   `yaml:"password,omitempty"`
	From     string   `yaml:"from,omitempty"`
	To       []string `yaml:"to,omitempty"`
}

// Email sends alerts through an SMTP server.
type Email struct {
	cfg EmailConfig
}

// NewEmail checks the config and returns a sender.
func NewEmail(cfg EmailConfig) (*Email, error) {
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	var missing []string
	if cfg.Host == "" {
		missing = append(missing, "host")
	}
	if cfg.From == "" {
		missing = append(missing, "from")
	}
	if len(cfg.To) == 0 {
		missing = append(missing, "to")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("alerts.email: %w: set %s", ErrNotConfigured, strings.Join(missing, ", "))
	}
	if (cfg.Username == "") != (cfg.Password == "") {
		return nil, fmt.Errorf("alerts.email: %w: set both username and password, or neither", ErrNotConfigured)
	}
	for _, a := range append([]string{cfg.From}, cfg.To...) {
		if !singleAddress(a) {
			return nil, fmt.Errorf("alerts.email: %q is not a single email address", a)
		}
	}
	return &Email{cfg: cfg}, nil
}

// Name implements Sender.
func (e *Email) Name() string { return "email" }

// Send implements Sender: one plain-text message to the configured recipients.
func (e *Email) Send(ctx context.Context, subject, body string) error {
	return e.deliver(ctx, e.cfg.To, e.compose(e.cfg.To, subject, body, ""))
}

// Recipients is the configured list of addresses every alert goes to.
func (e *Email) Recipients() []string { return append([]string(nil), e.cfg.To...) }

// SendTo sends one message to the given addresses, with an HTML part when html
// is not empty. The plain-text part always carries the whole message, so a
// client that shows only text -- or a lock screen -- loses nothing.
//
// The addresses come from accounts, not from the config file, so each is
// checked here as NewEmail checks the configured ones: a line break in an
// address would otherwise become a header of its own.
func (e *Email) SendTo(ctx context.Context, to []string, subject, text, html string) error {
	if len(to) == 0 {
		return nil
	}
	for _, a := range to {
		if !singleAddress(a) {
			return fmt.Errorf("%q is not a single email address", a)
		}
	}
	return e.deliver(ctx, to, e.compose(to, subject, text, html))
}

func singleAddress(a string) bool {
	return !strings.ContainsAny(a, "\r\n<>,") && strings.Contains(a, "@")
}

func (e *Email) deliver(ctx context.Context, to []string, msg []byte) error {
	addr := net.JoinHostPort(e.cfg.Host, strconv.Itoa(e.cfg.Port))
	tlsConf := &tls.Config{ServerName: e.cfg.Host, MinVersion: tls.VersionTLS12}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	// net/smtp takes no context, so the deadline goes on the connection.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if e.cfg.Port == 465 {
		conn = tls.Client(conn, tlsConf)
	}

	c, err := smtp.NewClient(conn, e.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp greeting from %s: %w", addr, err)
	}
	defer c.Close()

	if e.cfg.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsConf); err != nil {
				return fmt.Errorf("starttls with %s: %w", addr, err)
			}
		} else if !isLocal(e.cfg.Host) {
			// Who connected to which server, and as whom, is not something to
			// send across a network in the clear -- nor is the password.
			return fmt.Errorf("%s does not offer STARTTLS; refusing to send an alert in plain text "+
				"(use port 465, or a server that supports STARTTLS)", addr)
		}
	}

	if e.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, e.cfg.Host)); err != nil {
			return fmt.Errorf("smtp login as %s: %w", e.cfg.Username, err)
		}
	}
	if err := c.Mail(e.cfg.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp RCPT TO %s: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp end of message: %w", err)
	}
	return c.Quit()
}

// compose builds the RFC 5322 message: plain text, or plain text and HTML as
// multipart/alternative.
//
// The subject carries a hostname and an email address that came from a session
// report, so a line break in either is stripped before it reaches a header:
// otherwise a crafted hostname could add recipients or rewrite the message.
func (e *Email) compose(to []string, subject, body, html string) []byte {
	clean := strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", e.cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", clean))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	if html == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
		b.WriteString("\r\n")
		b.WriteString(crlf(body))
		return []byte(b.String())
	}
	boundary := mimeBoundary()
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", boundary)
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(crlf(body))
	fmt.Fprintf(&b, "\r\n--%s\r\n", boundary)
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(crlf(html))
	fmt.Fprintf(&b, "\r\n--%s--\r\n", boundary)
	return []byte(b.String())
}

// crlf gives a body the line endings SMTP wants. A line that is just "." ends
// DATA; smtp's DATA writer handles the dot, the endings are ours.
func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// mimeBoundary is random so no body can contain it by accident or design.
func mimeBoundary() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "argus-" + hex.EncodeToString(b[:])
}

func isLocal(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
