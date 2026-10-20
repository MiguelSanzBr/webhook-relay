package memory

import (
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

const window = 5 * time.Minute

func TestNonceUsedOnce(t *testing.T) {
	s := NewNonces(window)
	if !s.Reserve("n1", t0.Add(window), t0) {
		t.Fatal("primeira vez recusada")
	}
	if s.Reserve("n1", t0.Add(window), t0) {
		t.Fatal("mesmo nonce passou duas vezes")
	}
}

// Repasse falhou: o remetente vai tentar de novo com o mesmo nonce, então ele tem q ser liberado.
func TestReleaseAllowsRetry(t *testing.T) {
	s := NewNonces(window)
	s.Reserve("n1", t0.Add(window), t0)
	s.Release("n1")
	if !s.Reserve("n1", t0.Add(window), t0) {
		t.Fatal("depois do Release o nonce continuou preso")
	}
}

// Nonce só precisa ficar guardado enquanto o timestamp dele ainda passa na janela. Depois disso sai da memória.
func TestExpiredNoncesAreSwept(t *testing.T) {
	s := NewNonces(window)
	for _, n := range []string{"a", "b", "c"} {
		s.Reserve(n, t0.Add(window), t0)
	}

	now := t0.Add(6 * time.Minute)
	s.Reserve("d", now.Add(window), now)
	if len(s.seen) != 1 {
		t.Fatalf("%d nonces na memória, esperado só o d", len(s.seen))
	}
}
