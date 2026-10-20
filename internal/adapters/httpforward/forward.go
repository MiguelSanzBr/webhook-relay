// Package httpforward entrega o webhook no destino por HTTP, com retry.
package httpforward

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

// Forwarder é o app.Forwarder por HTTP. Erro de rede e timeout contam como
// temporários; o q fazer com o status do destino é do RetryPolicy.
type Forwarder struct {
	URL    string
	Client *http.Client
	Retry  domain.RetryPolicy
}

// New monta o Forwarder com um client q não segue redirect: 3xx volta como resposta e
// vira falha. Seguindo, o corpo ia pra outro host e um 302 virava GET sem corpo.
func New(url string, retry domain.RetryPolicy) *Forwarder {
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Forwarder{URL: url, Client: &http.Client{CheckRedirect: noRedirect}, Retry: retry}
}

func (f *Forwarder) Forward(ctx context.Context, w domain.SignedWebhook) error {
	for attempt := 1; ; attempt++ {
		temporary, err := f.try(ctx, w)
		if err == nil || !f.Retry.Again(attempt, temporary) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.Retry.Backoff(attempt)):
		}
	}
}

func (f *Forwarder) try(ctx context.Context, w domain.SignedWebhook) (temporary bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, f.Retry.AttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.URL, bytes.NewReader(w.Body))
	if err != nil {
		return false, err
	}
	if w.ContentType != "" {
		req.Header.Set("Content-Type", w.ContentType)
	}
	req.Header.Set("Idempotency-Key", w.Nonce)

	resp, err := f.Client.Do(req)
	if err != nil {
		return true, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	switch {
	case resp.StatusCode < 300:
		return false, nil
	case domain.TemporaryStatus(resp.StatusCode):
		return true, fmt.Errorf("destino: HTTP %d", resp.StatusCode)
	default:
		return false, fmt.Errorf("destino recusou: HTTP %d", resp.StatusCode)
	}
}
