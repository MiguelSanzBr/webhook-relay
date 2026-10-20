package memory

import (
	"testing"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

func allowN(l *Buckets, key string, now time.Time, n int) int {
	ok := 0
	for range n {
		if l.Allow(key, now) {
			ok++
		}
	}
	return ok
}

func TestRateLimiterBurstAndRefill(t *testing.T) {
	l := NewBuckets(domain.TokenBucket{Rate: 1, Burst: 3})

	if got := allowN(l, "ip1", t0, 5); got != 3 {
		t.Fatalf("rajada: %d liberadas, esperado 3", got)
	}
	now := t0.Add(time.Second)
	if got := allowN(l, "ip1", now, 5); got != 1 {
		t.Fatalf("depois de 1 s: %d liberadas, esperado 1", got)
	}
	// cada IP tem o próprio balde, um cliente barulhento não derruba os outros
	if got := allowN(l, "ip2", now, 5); got != 3 {
		t.Fatalf("outro IP: %d liberadas, esperado 3", got)
	}
	// parado 1 h não acumula 3600 fichas, o teto é a rajada
	now = now.Add(time.Hour)
	if got := allowN(l, "ip1", now, 10); got != 3 {
		t.Fatalf("depois de 1 h: %d liberadas, esperado 3", got)
	}
}

// Balde q já encheu de novo é igual a um balde novo, então pode sair da memória.
func TestIdleBucketsAreSwept(t *testing.T) {
	l := NewBuckets(domain.TokenBucket{Rate: 1, Burst: 3})
	for _, ip := range []string{"a", "b", "c"} {
		l.Allow(ip, t0)
	}

	l.Allow("d", t0.Add(10*time.Second))
	if len(l.buckets) != 1 {
		t.Fatalf("%d baldes na memória, esperado só o d", len(l.buckets))
	}
}
