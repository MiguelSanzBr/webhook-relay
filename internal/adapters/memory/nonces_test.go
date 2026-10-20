package memory

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

const window = 5 * time.Minute

var ctx = context.Background()

// clock é o relógio dos testes: começa em t0 e anda quando o teste manda.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func reserve(t *testing.T, s *Nonces, nonce string, keepUntil time.Time) bool {
	t.Helper()
	ok, err := s.Reserve(ctx, nonce, keepUntil)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestNonceUsedOnce(t *testing.T) {
	s := NewNonces(window, (&clock{t0}).now)
	if !reserve(t, s, "n1", t0.Add(window)) {
		t.Fatal("primeira vez recusada")
	}
	if reserve(t, s, "n1", t0.Add(window)) {
		t.Fatal("mesmo nonce passou duas vezes")
	}
}

// Repasse falhou: o remetente vai tentar de novo com o mesmo nonce, então ele tem q ser liberado.
func TestReleaseAllowsRetry(t *testing.T) {
	s := NewNonces(window, (&clock{t0}).now)
	reserve(t, s, "n1", t0.Add(window))
	if err := s.Release(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	if !reserve(t, s, "n1", t0.Add(window)) {
		t.Fatal("depois do Release o nonce continuou preso")
	}
}

// Nonce só precisa ficar guardado enquanto o timestamp dele ainda passa na janela. Depois disso sai da memória.
func TestExpiredNoncesAreSwept(t *testing.T) {
	c := &clock{t0}
	s := NewNonces(window, c.now)
	for _, n := range []string{"a", "b", "c"} {
		reserve(t, s, n, t0.Add(window))
	}

	c.t = t0.Add(6 * time.Minute)
	reserve(t, s, "d", c.t.Add(window))
	if len(s.seen) != 1 {
		t.Fatalf("%d nonces na memória, esperado só o d", len(s.seen))
	}
}

// A varredura só pode tirar o q já venceu. Nonce ainda na janela apagado é replay passando.
func TestSweepKeepsLiveNonces(t *testing.T) {
	c := &clock{t0}
	s := NewNonces(window, c.now)
	reserve(t, s, "velho", t0.Add(window))
	reserve(t, s, "vivo", t0.Add(10*time.Minute))

	c.t = t0.Add(6 * time.Minute) // passou uma janela: a próxima chamada varre
	if reserve(t, s, "vivo", t0.Add(10*time.Minute)) {
		t.Fatal("varredura apagou nonce q ainda vale: replay passou")
	}
	if !reserve(t, s, "velho", c.t.Add(window)) {
		t.Fatal("nonce vencido continuou preso")
	}
}

// Sem o lock, Reserve concorrente deixava o mesmo nonce passar duas vezes (e o -race acusa).
func TestConcurrentReserveOnlyOneWins(t *testing.T) {
	s := NewNonces(window, (&clock{t0}).now)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if ok, _ := s.Reserve(ctx, "n1", t0.Add(window)); ok {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d repasses do mesmo nonce, esperado 1", wins.Load())
	}
}
