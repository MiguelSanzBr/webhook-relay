package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/adapters/httpforward"
	"github.com/MiguelSanzBr/webhook-relay/internal/adapters/memory"
	"github.com/MiguelSanzBr/webhook-relay/internal/app"
	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

var secret = []byte("s3gredo")

type setup struct {
	h      http.Handler
	relay  *app.RelayWebhook
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

	s.relay = &app.RelayWebhook{
		Secret:    secret,
		Replay:    domain.ReplayPolicy{Window: 5 * time.Minute},
		Nonces:    memory.NewNonces(5*time.Minute, s.clock.now),
		Limiter:   memory.NewBuckets(domain.TokenBucket{Rate: 1, Burst: float64(burst)}, s.clock.now, 1000),
		Forwarder: httpforward.New(dst.URL, []byte("segredo-do-relay-pro-destino"), domain.RetryPolicy{MaxAttempts: 2, BackoffBase: time.Millisecond, AttemptTimeout: time.Second}),
		Clock:     s.clock.now,
		Timeout:   5 * time.Second,
	}
	h := &Handler{Relay: s.relay, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.h = h.Routes()
	return s
}

// signed monta um webhook assinado como o remetente faria.
func (s *setup) signed(ts time.Time, nonce, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	t := unix(ts)
	r.Header.Set("X-Webhook-Timestamp", t)
	r.Header.Set("X-Webhook-Nonce", nonce)
	r.Header.Set("X-Webhook-Signature", domain.Sign(secret, t, nonce, []byte(body)))
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

// Regressão da revisão de segurança: com ponto no nonce, a assinatura de um webhook capturado
// valia de novo com o começo do corpo movido pro nonce, e o destino recebia uma segunda entrega.
func TestDotShiftReplayIsRejected(t *testing.T) {
	s := newSetup(t, 100)
	ts := unix(s.clock.t)
	body := `{"pedido":42,"valor":10.50,"status":"pago"}`
	sig := domain.Sign(secret, ts, "evt_123", []byte(body))
	send := func(nonce, body string) int {
		r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
		r.Header.Set("X-Webhook-Timestamp", ts)
		r.Header.Set("X-Webhook-Nonce", nonce)
		r.Header.Set("X-Webhook-Signature", sig)
		return s.do(r)
	}

	i := strings.IndexByte(body, '.')
	codes := []int{send("evt_123", body), send("evt_123", body), send("evt_123."+body[:i], body[i+1:])}
	if codes[0] != http.StatusOK || codes[1] != http.StatusConflict || codes[2] != http.StatusUnauthorized {
		t.Fatalf("códigos %v, esperado [200 409 401]", codes)
	}
	if n := s.calls.Load(); n != 1 {
		t.Fatalf("destino recebeu %d entregas, esperado 1", n)
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

// Cliente do rate limit: IPv4 inteiro, IPv6 pelo /64 (com um /64 dava pra trocar de endereço
// a cada requisição e ganhar balde cheio, ou seja, limite nenhum) e IPv4 mapeado igual ao IPv4.
func TestRateLimitPerClient(t *testing.T) {
	cases := []struct {
		name, first, second string
		want                int
	}{
		{"mesmo IPv4", "192.0.2.1:1000", "192.0.2.1:2000", http.StatusTooManyRequests},
		{"outro IPv4 tem o próprio balde", "192.0.2.1:1000", "192.0.2.2:1000", http.StatusOK},
		{"mesmo /64", "[2001:db8::1]:1000", "[2001:db8::ffff:1]:1000", http.StatusTooManyRequests},
		{"outro /64", "[2001:db8::1]:1000", "[2001:db8:0:1::1]:1000", http.StatusOK},
		{"IPv4 mapeado é o mesmo IPv4", "192.0.2.1:1000", "[::ffff:192.0.2.1]:1000", http.StatusTooManyRequests},
		{"endereço q não é ip:porta", "@", "@", http.StatusTooManyRequests},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSetup(t, 1)
			r1, r2 := s.signed(s.clock.t, "n1", `{}`), s.signed(s.clock.t, "n2", `{}`)
			r1.RemoteAddr, r2.RemoteAddr = c.first, c.second
			if got := s.do(r1); got != http.StatusOK {
				t.Fatalf("primeira: HTTP %d", got)
			}
			if got := s.do(r2); got != c.want {
				t.Fatalf("segunda: HTTP %d, esperado %d", got, c.want)
			}
		})
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

// Store fora: 503, o remetente tenta de novo depois.
func TestStoreDownIs503(t *testing.T) {
	s := newSetup(t, 100)
	s.relay.Limiter = downLimiter{}
	if got := s.do(s.signed(s.clock.t, "n1", `{}`)); got != http.StatusServiceUnavailable {
		t.Fatalf("store fora: HTTP %d, esperado 503", got)
	}
}

type downLimiter struct{}

func (downLimiter) Allow(context.Context, string) (bool, error) {
	return false, errors.New("redis: conexão recusada")
}

func TestHealthz(t *testing.T) {
	s := newSetup(t, 1)
	if got := s.do(httptest.NewRequest(http.MethodGet, "/healthz", nil)); got != http.StatusNoContent {
		t.Fatalf("healthz: HTTP %d", got)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("conexão caiu") }
