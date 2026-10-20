package memory

import (
	"testing"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

func allowN(l *Buckets, key string, n int) int {
	ok := 0
	for range n {
		if allowed, err := l.Allow(ctx, key); err != nil {
			panic(err)
		} else if allowed {
			ok++
		}
	}
	return ok
}

func TestRateLimiterBurstAndRefill(t *testing.T) {
	c := &clock{t0}
	l := NewBuckets(domain.TokenBucket{Rate: 1, Burst: 3}, c.now)

	if got := allowN(l, "ip1", 5); got != 3 {
		t.Fatalf("rajada: %d liberadas, esperado 3", got)
	}
	c.t = t0.Add(time.Second)
	if got := allowN(l, "ip1", 5); got != 1 {
		t.Fatalf("depois de 1 s: %d liberadas, esperado 1", got)
	}
	// cada IP tem o próprio balde, um cliente barulhento não derruba os outros
	if got := allowN(l, "ip2", 5); got != 3 {
		t.Fatalf("outro IP: %d liberadas, esperado 3", got)
	}
	// parado 1 h não acumula 3600 fichas, o teto é a rajada
	c.t = c.t.Add(time.Hour)
	if got := allowN(l, "ip1", 10); got != 3 {
		t.Fatalf("depois de 1 h: %d liberadas, esperado 3", got)
	}
}

// Balde q já encheu de novo é igual a um balde novo, então pode sair da memória.
func TestIdleBucketsAreSwept(t *testing.T) {
	c := &clock{t0}
	l := NewBuckets(domain.TokenBucket{Rate: 1, Burst: 3}, c.now)
	for _, ip := range []string{"a", "b", "c"} {
		allowN(l, ip, 1)
	}

	c.t = t0.Add(10 * time.Second)
	allowN(l, "d", 1)
	if len(l.buckets) != 1 {
		t.Fatalf("%d baldes na memória, esperado só o d", len(l.buckets))
	}
}
