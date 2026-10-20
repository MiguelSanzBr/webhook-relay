package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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

var (
	secret    = []byte("s3gredo")
	fwdSecret = []byte("segredo-do-relay-pro-destino")
)

// received é o q o destino falso viu no último repasse.
type received struct {
	header http.Header
	body   string
}

type setup struct {
	h       http.Handler
	handler *Handler
	relay   *app.RelayWebhook
	clock   *fakeClock
	calls   *atomic.Int32
	status  *atomic.Int32 // o q o destino responde agora
	last    chan received // último repasse, buffer de 1
	logs    *bytes.Buffer
}

func newSetup(t *testing.T, burst int) *setup {
	s := &setup{clock: &fakeClock{t: time.Unix(1_700_000_000, 0)}, calls: &atomic.Int32{}, status: &atomic.Int32{}, last: make(chan received, 1), logs: &bytes.Buffer{}}
	s.status.Store(http.StatusOK)
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		select {
		case <-s.last: // descarta o anterior, fica só o último
		default:
		}
		s.last <- received{r.Header, string(body)}
		w.WriteHeader(int(s.status.Load()))
	}))
	t.Cleanup(dst.Close)

	s.relay = &app.RelayWebhook{
		Secrets:   [][]byte{secret},
		Replay:    domain.ReplayPolicy{Window: 5 * time.Minute},
		Nonces:    memory.NewNonces(5*time.Minute, s.clock.now),
		Limiter:   memory.NewBuckets(domain.TokenBucket{Rate: 1, Burst: float64(burst)}, s.clock.now, 1000),
		Forwarder: httpforward.New(dst.URL, fwdSecret, domain.RetryPolicy{MaxAttempts: 2, BackoffBase: time.Millisecond, AttemptTimeout: time.Second}),
		Clock:     s.clock.now,
		Timeout:   5 * time.Second,
	}
	s.handler = &Handler{Relay: s.relay, RetryAfter: time.Second, Log: slog.New(slog.NewJSONHandler(s.logs, nil))}
	s.h = s.handler.Routes()
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

func (s *setup) do(r *http.Request) int { return s.rec(r).Code }

func (s *setup) rec(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	return rec
}

// Cada recusa num setup próprio: nada disso pode chegar no destino.
func TestWebhookChecks(t *testing.T) {
	cases := []struct {
		name string
		req  func(s *setup) *http.Request
		want int
	}{
		{"corpo trocado depois de assinar", func(s *setup) *http.Request {
			r := s.signed(s.clock.t, "n1", `{"valor":10}`)
			r.Body = io.NopCloser(strings.NewReader(`{"valor":10000}`))
			return r
		}, http.StatusUnauthorized},
		{"sem headers de assinatura", func(*setup) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
		}, http.StatusUnauthorized},
		{"assinado certo mas com 6 min", func(s *setup) *http.Request {
			return s.signed(s.clock.t.Add(-6*time.Minute), "n1", `{}`)
		}, http.StatusUnauthorized},
		{"timestamp q não é número", func(s *setup) *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
			r.Header.Set("X-Webhook-Timestamp", "ontem")
			r.Header.Set("X-Webhook-Nonce", "n1")
			r.Header.Set("X-Webhook-Signature", domain.Sign(secret, "ontem", "n1", []byte(`{}`)))
			return r
		}, http.StatusUnauthorized},
		{"corpo maior q 1 MiB", func(s *setup) *http.Request {
			return s.signed(s.clock.t, "n1", strings.Repeat("a", maxBody+1))
		}, http.StatusRequestEntityTooLarge},
		{"conexão caiu lendo o corpo", func(s *setup) *http.Request {
			r := s.signed(s.clock.t, "n1", `{}`)
			r.Body = io.NopCloser(errReader{})
			return r
		}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSetup(t, 100)
			if got := s.do(c.req(s)); got != c.want {
				t.Fatalf("HTTP %d, esperado %d", got, c.want)
			}
			if n := s.calls.Load(); n != 0 {
				t.Fatalf("destino recebeu %d webhooks, esperado nenhum", n)
			}
		})
	}
}

// O q chega no destino: o corpo igual, o Content-Type, o nonce como Idempotency-Key e a
// assinatura do relay (não a do remetente). E o remetente recebe o JSON de status.
func TestForwardedRequest(t *testing.T) {
	s := newSetup(t, 100)
	r := s.signed(s.clock.t, "evt_1", `{"pedido":42}`)
	senderSig := r.Header.Get("X-Webhook-Signature")
	rec := s.rec(r)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != `{"status":"repassado"}`+"\n" {
		t.Fatalf("resposta: HTTP %d %s %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}

	got := <-s.last
	h := got.header
	if got.body != `{"pedido":42}` || h.Get("Content-Type") != "application/json" || h.Get("Idempotency-Key") != "evt_1" {
		t.Fatalf("repasse chegou errado: headers=%v body=%s", h, got.body)
	}
	sig := h.Get("X-Webhook-Signature")
	if sig == senderSig || !domain.Verify(fwdSecret, h.Get("X-Webhook-Timestamp"), "evt_1", []byte(got.body), sig) {
		t.Fatalf("repasse sem a assinatura do relay: %v", h)
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
	s.handler.RetryAfter = 2500 * time.Millisecond // RATE=0.4: a próxima ficha leva 2,5 s
	var last *httptest.ResponseRecorder
	codes := []int{}
	for i := range 3 {
		last = s.rec(s.signed(s.clock.t, "n"+unix(time.Unix(int64(i), 0)), `{}`))
		codes = append(codes, last.Code)
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("códigos %v, esperado [200 200 429]", codes)
	}
	// Retry-After fixo em 1 com RATE baixo fazia o cliente voltar cedo e levar outro 429
	if got := last.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After=%q, esperado 3 (2,5 s pra cima)", got)
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

// Cada request tem um id: o de fora se for curto e limpo, senão o relay cria um. Ele volta no
// header e vai em toda linha de log do request, inclusive na linha final com status.
func TestRequestIDGoesToLog(t *testing.T) {
	s := newSetup(t, 100)
	r := s.signed(s.clock.t, "n1", `{}`)
	r.Header.Set("X-Request-Id", "req-abc_1")
	if got := s.rec(r).Header().Get("X-Request-Id"); got != "req-abc_1" {
		t.Fatalf("X-Request-Id=%q, esperado o do cliente", got)
	}
	if !strings.Contains(s.logs.String(), `"req_id":"req-abc_1"`) || !strings.Contains(s.logs.String(), `"status":200`) {
		t.Fatalf("log sem req_id ou status: %s", s.logs)
	}

	// assinatura errada: o warn também leva o id
	s.logs.Reset()
	r = s.signed(s.clock.t, "n2", `{}`)
	r.Header.Set("X-Webhook-Signature", "sha256=00")
	r.Header.Set("X-Request-Id", strings.Repeat("x", 200)) // comprido demais: o relay cria outro
	id := s.rec(r).Header().Get("X-Request-Id")
	if id == "" || len(id) > 128 {
		t.Fatalf("id novo %q", id)
	}
	if !logLine(s.logs.String(), "assinatura inválida", `"req_id":"`+id+`"`) {
		t.Fatalf("warn sem o req_id %s: %s", id, s.logs)
	}
}

// /debug/vars conta os status por rota: dá pra responder quantos 401/409/502 teve.
func TestStatusMetrics(t *testing.T) {
	s := newSetup(t, 100)
	before := statusCount(t, s, "POST /webhook 200")
	s.do(s.signed(s.clock.t, "n1", `{}`))
	if got := statusCount(t, s, "POST /webhook 200"); got != before+1 {
		t.Fatalf("contador de 200 foi de %d pra %d", before, got)
	}
}

func statusCount(t *testing.T, s *setup, key string) int {
	t.Helper()
	var vars struct {
		Status map[string]int `json:"http_status"`
	}
	if err := json.Unmarshal(s.rec(httptest.NewRequest(http.MethodGet, "/debug/vars", nil)).Body.Bytes(), &vars); err != nil {
		t.Fatal(err)
	}
	return vars.Status[key]
}

// logLine diz se alguma linha do log tem todos os pedaços.
func logLine(logs string, parts ...string) bool {
	for line := range strings.Lines(logs) {
		if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) }) {
			return true
		}
	}
	return false
}
