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

type fakeNonces struct {
	seen          map[string]bool
	err           error // store fora: Reserve e Release falham
	releaseCtxErr error // ctx q o Release recebeu já estava cancelado?
}

func (f *fakeNonces) Reserve(_ context.Context, nonce string, _ time.Time) (bool, error) {
	if f.err != nil || f.seen[nonce] {
		return false, f.err
	}
	f.seen[nonce] = true
	return true, nil
}

func (f *fakeNonces) Release(ctx context.Context, nonce string) error {
	f.releaseCtxErr = ctx.Err()
	delete(f.seen, nonce)
	return f.err
}

type fakeLimiter struct {
	ok  bool
	err error
}

func (f fakeLimiter) Allow(context.Context, string) (bool, error) { return f.ok, f.err }

type fakeForwarder struct {
	err         error
	calls       int
	nonce, ct   string
	body        string
	hasDeadline bool
	ctxErr      error
}

func (f *fakeForwarder) Forward(ctx context.Context, w domain.SignedWebhook) error {
	f.calls++
	f.nonce, f.ct, f.body = w.Nonce, w.ContentType, string(w.Body)
	_, f.hasDeadline = ctx.Deadline()
	f.ctxErr = ctx.Err()
	return f.err
}

func newRelay() (*RelayWebhook, *fakeNonces, *fakeForwarder) {
	nonces, fwd := &fakeNonces{seen: map[string]bool{}}, &fakeForwarder{}
	return &RelayWebhook{
		Secrets:   [][]byte{secret},
		Replay:    domain.ReplayPolicy{Window: 5 * time.Minute},
		Nonces:    nonces,
		Limiter:   fakeLimiter{ok: true},
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
		uc.Limiter = fakeLimiter{ok: !c.limited}
		// read nil estoura se for chamado: limite vem antes de ler o corpo
		err := uc.Relay(context.Background(), "ip1", c.read)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v, esperado %v", c.name, err, c.want)
		}
		// nonce só é guardado depois da assinatura, senão lixo enche a memória
		if len(nonces.seen) != 0 || fwd.calls != 0 {
			t.Errorf("%s: nonces=%v repasses=%d, esperado nada", c.name, nonces.seen, fwd.calls)
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
	if nonces.seen["n1"] {
		t.Fatal("nonce ficou preso depois do repasse falhar")
	}

	fwd.err = nil
	if err := uc.Relay(context.Background(), "ip1", signed(t0, "n1", `{}`)); err != nil {
		t.Fatalf("retry do remetente: %v", err)
	}
}

// Store fora (Redis caiu, por ex.): fail-closed. Sem saber se o nonce é novo, nada é repassado.
func TestStoreDownFailsClosed(t *testing.T) {
	down := errors.New("redis: conexão recusada")

	uc, _, _ := newRelay()
	uc.Limiter = fakeLimiter{err: down}
	// read nil estoura se for chamado: sem limite confiável nem lê o corpo
	if err := uc.Relay(context.Background(), "ip1", nil); !errors.Is(err, ErrUnavailable) || !errors.Is(err, down) {
		t.Fatalf("limiter fora: err=%v, esperado ErrUnavailable com a causa", err)
	}

	uc, nonces, fwd := newRelay()
	nonces.err = down
	if err := uc.Relay(context.Background(), "ip1", signed(t0, "n1", `{}`)); !errors.Is(err, ErrUnavailable) || !errors.Is(err, down) {
		t.Fatalf("nonces fora: err=%v, esperado ErrUnavailable com a causa", err)
	}
	if fwd.calls != 0 {
		t.Fatalf("repassou %d vezes sem conseguir guardar o nonce", fwd.calls)
	}
}

// Release falhou depois do repasse falhar: o nonce fica preso, então o erro tem q aparecer.
func TestFailedReleaseIsReported(t *testing.T) {
	uc, nonces, fwd := newRelay()
	fwd.err = errors.New("destino: HTTP 503")
	stuck := errors.New("redis: timeout")

	uc.Nonces = &releaseFails{fakeNonces: nonces, err: stuck}
	err := uc.Relay(context.Background(), "ip1", signed(t0, "n1", `{}`))
	if !errors.As(err, new(*ForwardError)) || !errors.Is(err, fwd.err) || !errors.Is(err, stuck) {
		t.Fatalf("err=%v, esperado ForwardError com as duas causas", err)
	}
}

type releaseFails struct {
	*fakeNonces
	err error
}

func (r *releaseFails) Release(context.Context, string) error { return r.err }

// O ctx do remetente chega no repasse: se ele desistiu, o repasse para. Mas liberar o nonce
// acontece mesmo assim, senão o retry dele levava 409.
func TestCallerContextReachesForward(t *testing.T) {
	uc, nonces, fwd := newRelay()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fwd.err = context.Canceled

	if err := uc.Relay(ctx, "ip1", signed(t0, "n1", `{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, esperado context.Canceled", err)
	}
	if !errors.Is(fwd.ctxErr, context.Canceled) {
		t.Fatalf("repasse viu ctx.Err()=%v, o ctx do remetente não chegou", fwd.ctxErr)
	}
	if nonces.releaseCtxErr != nil || nonces.seen["n1"] {
		t.Fatalf("Release com ctx cancelado (%v) ou nonce preso", nonces.releaseCtxErr)
	}
}

// Rotação: durante a troca valem o segredo velho e o novo. Fora da lista, 401.
func TestRelayAcceptsAnyActiveSecret(t *testing.T) {
	uc, _, fwd := newRelay()
	novo := []byte("segredo-novo")
	uc.Secrets = [][]byte{secret, novo}
	withSecret := func(s []byte, nonce string) func() (domain.SignedWebhook, error) {
		return func() (domain.SignedWebhook, error) {
			w, _ := signed(t0, nonce, `{}`)()
			w.Signature = domain.Sign(s, w.Timestamp, nonce, w.Body)
			return w, nil
		}
	}
	if err := uc.Relay(context.Background(), "ip1", withSecret(secret, "n1")); err != nil {
		t.Fatalf("segredo velho: %v", err)
	}
	if err := uc.Relay(context.Background(), "ip1", withSecret(novo, "n2")); err != nil {
		t.Fatalf("segredo novo: %v", err)
	}
	if err := uc.Relay(context.Background(), "ip1", withSecret([]byte("outro"), "n3")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("segredo fora da lista: err=%v", err)
	}
	if fwd.calls != 2 {
		t.Fatalf("%d repasses, esperado 2", fwd.calls)
	}
}
