package memory

import (
	"sync"
	"sync/atomic"
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

// Balde meio vazio não pode sair na varredura: voltava cheio e o cliente ganhava rajada nova.
func TestSweepKeepsPartialBuckets(t *testing.T) {
	c := &clock{t0}
	l := NewBuckets(domain.TokenBucket{Rate: 1, Burst: 3}, c.now)
	allowN(l, "x", 3) // esvazia em t0
	c.t = t0.Add(2 * time.Second)
	allowN(l, "x", 1)             // fica com 1
	c.t = t0.Add(3 * time.Second) // Refill passou: esta chamada varre, x tem 2 de 3
	if got := allowN(l, "x", 5); got != 2 {
		t.Fatalf("depois da varredura: %d liberadas, esperado 2 (o balde parcial sumiu)", got)
	}
}

// Sem o lock, Allow concorrente liberava mais q a rajada (e o -race acusa).
func TestConcurrentAllowRespectsBurst(t *testing.T) {
	l := NewBuckets(domain.TokenBucket{Rate: 1, Burst: 20}, (&clock{t0}).now)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if allowed, _ := l.Allow(ctx, "ip1"); allowed {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 20 {
		t.Fatalf("%d liberadas ao mesmo tempo, esperado 20", ok.Load())
	}
}
