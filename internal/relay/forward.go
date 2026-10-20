package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"
)

// Forwarder repassa o webhook pro destino. Tenta de novo só quando faz sentido:
// rede, timeout, 429 e 5xx. Outro 4xx é erro do pedido e repetir não muda nada.
type Forwarder struct {
	URL         string
	Client      *http.Client
	MaxAttempts int
	BackoffBase time.Duration
}

func (f *Forwarder) Forward(ctx context.Context, nonce, contentType string, body []byte) error {
	for attempt := 1; ; attempt++ {
		retry, err := f.try(ctx, nonce, contentType, body)
		if err == nil || !retry || attempt >= f.MaxAttempts {
			return err
		}
		// backoff exponencial com jitter, metade fixa e metade aleatória, pra os retries não baterem todos juntos
		d := f.BackoffBase << min(attempt-1, 20)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d/2 + rand.N(d/2+1)):
		}
	}
}

func (f *Forwarder) try(ctx context.Context, nonce, contentType string, body []byte) (retry bool, err error) {
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
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, fmt.Errorf("destino: HTTP %d", resp.StatusCode)
	default:
		return false, fmt.Errorf("destino recusou: HTTP %d", resp.StatusCode)
	}
}
