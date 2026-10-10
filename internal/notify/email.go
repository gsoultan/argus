package notify

import (
	"context"
	"crypto/tls"
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
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// Username and Password are optional; with neither, no AUTH is sent.
	// Put the password in the environment or a file: "${file:/path}".
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
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
		if strings.ContainsAny(a, "\r\n<>,") || !strings.Contains(a, "@") {
			return nil, fmt.Errorf("alerts.email: %q is not a single email address", a)
		}
	}
	return &Email{cfg: cfg}, nil
}

// Name implements Sender.
func (e *Email) Name() string { return "email" }

// Send implements Sender.
func (e *Email) Send(ctx context.Context, subject, body string) error {
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
	for _, to := range e.cfg.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("smtp RCPT TO %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(e.compose(subject, body)); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp end of message: %w", err)
	}
	return c.Quit()
}

// compose builds the RFC 5322 message.
//
// The subject carries a hostname and an email address that came from a session
// report, so a line break in either is stripped before it reaches a header:
// otherwise a crafted hostname could add recipients or rewrite the message.
func (e *Email) compose(subject, body string) []byte {
	clean := strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", e.cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(e.cfg.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", clean))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("\r\n")
	// SMTP wants CRLF line endings, and a line that is just "." ends DATA;
	// smtp's DATA writer handles the dot, the endings are ours.
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}

func isLocal(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
