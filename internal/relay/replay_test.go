package relay

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestReplayGuard(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := NewReplayGuard(5*time.Minute, clock.now)
	ts := unix(clock.t)

	if err := g.Reserve(ts, "n1"); err != nil {
		t.Fatalf("primeira vez: %v", err)
	}
	if err := g.Reserve(ts, "n1"); !errors.Is(err, ErrReplay) {
		t.Fatalf("mesmo nonce de novo: err=%v, esperado ErrReplay", err)
	}

	cases := []struct{ name, ts string }{
		{"6 min no passado", unix(clock.t.Add(-6 * time.Minute))},
		{"6 min no futuro", unix(clock.t.Add(6 * time.Minute))},
		{"não é número", "ontem"},
	}
	for _, c := range cases {
		if err := g.Reserve(c.ts, "novo-"+c.name); !errors.Is(err, ErrStale) {
			t.Errorf("%s: err=%v, esperado ErrStale", c.name, err)
		}
	}
}

// Repasse falhou: o remetente vai tentar de novo com o mesmo nonce, então ele tem q ser liberado.
func TestReleaseAllowsRetry(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := NewReplayGuard(5*time.Minute, clock.now)
	ts := unix(clock.t)

	if err := g.Reserve(ts, "n1"); err != nil {
		t.Fatal(err)
	}
	g.Release("n1")
	if err := g.Reserve(ts, "n1"); err != nil {
		t.Fatalf("depois do Release: %v", err)
	}
}

// Nonce só precisa ficar guardado enquanto o timestamp dele ainda passa na janela. Depois disso sai da memória.
func TestExpiredNoncesAreSwept(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := NewReplayGuard(5*time.Minute, clock.now)
	for _, n := range []string{"a", "b", "c"} {
		if err := g.Reserve(unix(clock.t), n); err != nil {
			t.Fatal(err)
		}
	}

	clock.advance(6 * time.Minute)
	if err := g.Reserve(unix(clock.t), "d"); err != nil {
		t.Fatal(err)
	}
	if len(g.seen) != 1 {
		t.Fatalf("%d nonces na memória, esperado só o d", len(g.seen))
	}
}
