package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

// Forwarder repassa o webhook pro destino. Erro de rede e timeout contam como
// temporários; o q fazer com o status do destino é do RetryPolicy.
type Forwarder struct {
	URL    string
	Client *http.Client
	Retry  domain.RetryPolicy
}

func (f *Forwarder) Forward(ctx context.Context, nonce, contentType string, body []byte) error {
	for attempt := 1; ; attempt++ {
		temporary, err := f.try(ctx, nonce, contentType, body)
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

func (f *Forwarder) try(ctx context.Context, nonce, contentType string, body []byte) (temporary bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.URL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Idempotency-Key", nonce)

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
