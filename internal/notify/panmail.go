package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/gsmail"
	panmail "github.com/gsoultan/panmail-sdk"
)

// panmailSender is a gsmail.Sender that hands mail to a Panmail gateway over
// its HTTP API.
//
// The gateway queues the message and owns delivery from there, so a nil error
// means Panmail has it, not that it has arrived. Every message carries an
// idempotency key: a send whose outcome is unknown -- a timeout, a dropped
// connection -- is asked again with the same key, and the gateway answers
// with the message it already queued rather than sending a second copy.
type panmailSender struct {
	gsmail.BaseProvider
	client     *panmail.Client
	baseURL    string
	providerID string
	http       *http.Client
}

func newPanmailSender(cfg PanmailConfig) (*panmailSender, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	hc := &http.Client{Timeout: 20 * time.Second}
	c, err := panmail.New(base, cfg.APIKey,
		panmail.WithHTTPClient(hc),
		panmail.WithTransportRetries(2),
		panmail.WithRateLimitRetries(1))
	if err != nil {
		return nil, err
	}
	return &panmailSender{client: c, baseURL: base, providerID: strings.TrimSpace(cfg.ProviderID), http: hc}, nil
}

// Send implements gsmail.Sender.
func (p *panmailSender) Send(ctx context.Context, e gsmail.Email) error {
	if len(e.Envelope) > 0 {
		// Panmail's API takes one recipient list for headers and delivery
		// alike, so it cannot honour a separate envelope.
		return gsmail.NonRetryable(errors.New("panmail: a separate envelope is not supported"))
	}
	_, err := p.client.Send(ctx, panmail.Message{
		ProviderID:     p.providerID,
		From:           e.From,
		To:             e.To,
		Cc:             e.Cc,
		Bcc:            e.Bcc,
		ReplyTo:        e.ReplyTo,
		Subject:        e.Subject,
		Text:           string(e.Body),
		HTML:           string(e.HTMLBody),
		IdempotencyKey: newKey(),
	})
	if err != nil {
		return fmt.Errorf("send through Panmail at %s: %w", p.baseURL, explainPanmail(err))
	}
	return nil
}

// Ping implements gsmail.Sender: is the gateway up and ready to queue mail.
func (p *panmailSender) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach Panmail at %s: %w", p.baseURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Panmail at %s is not ready (http %d)", p.baseURL, resp.StatusCode)
	}
	return nil
}

// explainPanmail adds the next step to the refusals an administrator can fix
// from Panmail's dashboard. The SDK's own message stays in the chain.
func explainPanmail(err error) error {
	var (
		auth       *panmail.AuthError
		suppressed *panmail.SuppressedRecipientError
		api        *panmail.APIError
	)
	switch {
	case errors.As(err, &auth):
		return fmt.Errorf("%w (check the API key, and that it has the email:send scope)", err)
	case errors.As(err, &suppressed):
		return fmt.Errorf("%w (lift the suppression in Panmail, or remove the address from the recipients)", err)
	case errors.As(err, &api) && api.Code == "invalid_argument":
		return fmt.Errorf("%w (check the provider id, and that the From address is an allowed sender for that provider)", err)
	}
	return err
}
