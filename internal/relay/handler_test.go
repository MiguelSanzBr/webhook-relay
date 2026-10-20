package relay

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var secret = []byte("s3gredo")

type setup struct {
	h      http.Handler
	clock  *fakeClock
	calls  *atomic.Int32
	status *atomic.Int32 // o q o destino responde agora
}

func newSetup(t *testing.T, burst int) *setup {
	s := &setup{clock: &fakeClock{t: time.Unix(1_700_000_000, 0)}, calls: &atomic.Int32{}, status: &atomic.Int32{}}
	s.status.Store(http.StatusOK)
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.calls.Add(1)
		w.WriteHeader(int(s.status.Load()))
	}))
	t.Cleanup(dst.Close)

	h := &Handler{
		Secret:  secret,
		Guard:   NewReplayGuard(5*time.Minute, s.clock.now),
		Limiter: NewRateLimiter(1, burst, s.clock.now),
		Forward: &Forwarder{URL: dst.URL, Client: dst.Client(), MaxAttempts: 2, BackoffBase: time.Millisecond},
		Timeout: 5 * time.Second,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.h = h.Routes()
	return s
}

// signed monta um webhook assinado como o remetente faria.
func (s *setup) signed(ts time.Time, nonce, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	t := unix(ts)
	r.Header.Set("X-Webhook-Timestamp", t)
	r.Header.Set("X-Webhook-Nonce", nonce)
	r.Header.Set("X-Webhook-Signature", Sign(secret, t, nonce, []byte(body)))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func (s *setup) do(r *http.Request) int {
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	return rec.Code
}

func TestWebhookChecks(t *testing.T) {
	s := newSetup(t, 100)
	now := s.clock.t

	tampered := s.signed(now, "n-adulterado", `{"valor":10}`)
	tampered.Body = io.NopCloser(strings.NewReader(`{"valor":10000}`))
	unsigned := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
	broken := s.signed(now, "n-quebrado", `{}`)
	broken.Body = io.NopCloser(errReader{})

	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"assinado e na janela", s.signed(now, "n1", `{"pedido":42}`), http.StatusOK},
		{"mesmo webhook de novo", s.signed(now, "n1", `{"pedido":42}`), http.StatusConflict},
		{"corpo trocado depois de assinar", tampered, http.StatusUnauthorized},
		{"sem headers de assinatura", unsigned, http.StatusUnauthorized},
		{"assinado certo mas com 6 min", s.signed(now.Add(-6*time.Minute), "n-velho", `{}`), http.StatusUnauthorized},
		{"corpo maior q 1 MiB", s.signed(now, "n-grande", strings.Repeat("a", maxBody+1)), http.StatusRequestEntityTooLarge},
		{"conexão caiu lendo o corpo", broken, http.StatusBadRequest},
	}
	for _, c := range cases {
		if got := s.do(c.req); got != c.want {
			t.Errorf("%s: HTTP %d, esperado %d", c.name, got, c.want)
		}
	}
	if n := s.calls.Load(); n != 1 {
		t.Fatalf("destino recebeu %d webhooks, esperado só o válido", n)
	}
}

func TestRateLimitPerIP(t *testing.T) {
	s := newSetup(t, 2)
	codes := []int{}
	for i := range 3 {
		codes = append(codes, s.do(s.signed(s.clock.t, "n"+unix(time.Unix(int64(i), 0)), `{}`)))
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("códigos %v, esperado [200 200 429]", codes)
	}
}

// Destino fora: o relay responde 502 e libera o nonce. Quando o remetente tenta de novo com o
// mesmo webhook e o destino já voltou, passa. Se o nonce ficasse preso, o retry legítimo levava 409.
func TestFailedForwardLetsSenderRetry(t *testing.T) {
	s := newSetup(t, 100)
	s.status.Store(http.StatusServiceUnavailable)
	if got := s.do(s.signed(s.clock.t, "n1", `{}`)); got != http.StatusBadGateway {
		t.Fatalf("destino fora: HTTP %d, esperado 502", got)
	}

	s.status.Store(http.StatusOK)
	if got := s.do(s.signed(s.clock.t, "n1", `{}`)); got != http.StatusOK {
		t.Fatalf("retry do remetente: HTTP %d, esperado 200", got)
	}
}

func TestHealthz(t *testing.T) {
	s := newSetup(t, 1)
	if got := s.do(httptest.NewRequest(http.MethodGet, "/healthz", nil)); got != http.StatusNoContent {
		t.Fatalf("healthz: HTTP %d", got)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("conexão caiu") }
