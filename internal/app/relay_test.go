package app

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

var secret = []byte("s3gredo")

var t0 = time.Unix(1_700_000_000, 0)

type fakeNonces map[string]bool

func (f fakeNonces) Reserve(nonce string, _, _ time.Time) bool {
	if f[nonce] {
		return false
	}
	f[nonce] = true
	return true
}

func (f fakeNonces) Release(nonce string) { delete(f, nonce) }

type fakeLimiter bool

func (f fakeLimiter) Allow(string, time.Time) bool { return bool(f) }

type fakeForwarder struct {
	err         error
	calls       int
	nonce, ct   string
	body        string
	hasDeadline bool
}

func (f *fakeForwarder) Forward(ctx context.Context, w domain.SignedWebhook) error {
	f.calls++
	f.nonce, f.ct, f.body = w.Nonce, w.ContentType, string(w.Body)
	_, f.hasDeadline = ctx.Deadline()
	return f.err
}

func newRelay() (*RelayWebhook, fakeNonces, *fakeForwarder) {
	nonces, fwd := fakeNonces{}, &fakeForwarder{}
	return &RelayWebhook{
		Secret:    secret,
		Replay:    domain.ReplayPolicy{Window: 5 * time.Minute},
		Nonces:    nonces,
		Limiter:   fakeLimiter(true),
		Forwarder: fwd,
		Clock:     func() time.Time { return t0 },
		Timeout:   time.Second,
	}, nonces, fwd
}

func signed(ts time.Time, nonce, body string) func() (domain.SignedWebhook, error) {
	t := strconv.FormatInt(ts.Unix(), 10)
	return func() (domain.SignedWebhook, error) {
		return domain.SignedWebhook{
			Timestamp:   t,
			Nonce:       nonce,
			Signature:   domain.Sign(secret, t, nonce, []byte(body)),
			ContentType: "application/json",
			Body:        []byte(body),
		}, nil
	}
}

func TestRelayForwardsOnce(t *testing.T) {
	uc, _, fwd := newRelay()
	if err := uc.Relay(context.Background(), "ip1", signed(t0, "n1", `{"pedido":42}`)); err != nil {
		t.Fatal(err)
	}
	if fwd.nonce != "n1" || fwd.ct != "application/json" || fwd.body != `{"pedido":42}` {
		t.Fatalf("repasse chegou errado: %+v", fwd)
	}
	// o remetente não pode prender o relay: o repasse inteiro, retries incluídos, tem prazo
	if !fwd.hasDeadline {
		t.Fatal("repasse sem prazo")
	}
	if err := uc.Relay(context.Background(), "ip1", signed(t0, "n1", `{"pedido":42}`)); !errors.Is(err, domain.ErrReplay) {
		t.Fatalf("mesmo webhook de novo: err=%v, esperado ErrReplay", err)
	}
	if fwd.calls != 1 {
		t.Fatalf("destino recebeu %d, esperado 1", fwd.calls)
	}
}

func TestRelayRejects(t *testing.T) {
	errRead := errors.New("corpo grande demais")
	tampered := func() (domain.SignedWebhook, error) {
		w, _ := signed(t0, "n-adulterado", `{"valor":10}`)()
		w.Body = []byte(`{"valor":10000}`)
		return w, nil
	}

	cases := []struct {
		name    string
		limited bool
		read    func() (domain.SignedWebhook, error)
		want    error
	}{
		{"estourou o limite", true, nil, ErrRateLimited},
		{"erro lendo o webhook", false, func() (domain.SignedWebhook, error) { return domain.SignedWebhook{}, errRead }, errRead},
		{"corpo trocado depois de assinar", false, tampered, ErrBadSignature},
		{"assinado certo mas com 6 min", false, signed(t0.Add(-6*time.Minute), "n-velho", `{}`), domain.ErrStale},
	}
	for _, c := range cases {
		uc, nonces, fwd := newRelay()
		uc.Limiter = fakeLimiter(!c.limited)
		// read nil estoura se for chamado: limite vem antes de ler o corpo
		err := uc.Relay(context.Background(), "ip1", c.read)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v, esperado %v", c.name, err, c.want)
		}
		// nonce só é guardado depois da assinatura, senão lixo enche a memória
		if len(nonces) != 0 || fwd.calls != 0 {
			t.Errorf("%s: nonces=%v repasses=%d, esperado nada", c.name, nonces, fwd.calls)
		}
	}
}

// Destino fora: o nonce volta pra o retry do remetente com o mesmo webhook passar.
func TestFailedForwardReleasesNonce(t *testing.T) {
	uc, nonces, fwd := newRelay()
	down := errors.New("destino: HTTP 503")
	fwd.err = down

	err := uc.Relay(context.Background(), "ip1", signed(t0, "n1", `{}`))
	var fe *ForwardError
	if !errors.As(err, &fe) || !errors.Is(err, down) || err.Error() != down.Error() {
		t.Fatalf("err=%v, esperado ForwardError com a causa", err)
	}
	if nonces["n1"] {
		t.Fatal("nonce ficou preso depois do repasse falhar")
	}

	fwd.err = nil
	if err := uc.Relay(context.Background(), "ip1", signed(t0, "n1", `{}`)); err != nil {
		t.Fatalf("retry do remetente: %v", err)
	}
}
