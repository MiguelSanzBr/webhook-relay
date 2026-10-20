package relay

import (
	"testing"
	"time"
)

func allowN(l *RateLimiter, key string, n int) int {
	ok := 0
	for range n {
		if l.Allow(key) {
			ok++
		}
	}
	return ok
}

func TestRateLimiterBurstAndRefill(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewRateLimiter(1, 3, clock.now)

	if got := allowN(l, "ip1", 5); got != 3 {
		t.Fatalf("rajada: %d liberadas, esperado 3", got)
	}
	clock.advance(time.Second)
	if got := allowN(l, "ip1", 5); got != 1 {
		t.Fatalf("depois de 1 s: %d liberadas, esperado 1", got)
	}
	// cada IP tem o próprio balde, um cliente barulhento não derruba os outros
	if got := allowN(l, "ip2", 5); got != 3 {
		t.Fatalf("outro IP: %d liberadas, esperado 3", got)
	}
	// parado 1 h não acumula 3600 fichas, o teto é a rajada
	clock.advance(time.Hour)
	if got := allowN(l, "ip1", 10); got != 3 {
		t.Fatalf("depois de 1 h: %d liberadas, esperado 3", got)
	}
}

// Balde q já encheu de novo é igual a um balde novo, então pode sair da memória.
func TestIdleBucketsAreSwept(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewRateLimiter(1, 3, clock.now)
	for _, ip := range []string{"a", "b", "c"} {
		l.Allow(ip)
	}

	clock.advance(10 * time.Second)
	l.Allow("d")
	if len(l.buckets) != 1 {
		t.Fatalf("%d baldes na memória, esperado só o d", len(l.buckets))
	}
}
