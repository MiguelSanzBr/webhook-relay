package domain

import (
	"testing"
	"time"
)

func TestTokenBucket(t *testing.T) {
	p := TokenBucket{Rate: 1, Burst: 3}
	now := time.Unix(1_700_000_000, 0)
	b := p.New(now)
	take := func(n int) int {
		ok := 0
		for range n {
			if p.Take(&b, now) {
				ok++
			}
		}
		return ok
	}

	if got := take(5); got != 3 {
		t.Fatalf("rajada: %d liberadas, esperado 3", got)
	}
	now = now.Add(time.Second)
	if got := take(5); got != 1 {
		t.Fatalf("depois de 1 s: %d liberadas, esperado 1", got)
	}
	// parado 1 h não acumula 3600 fichas, o teto é a rajada
	now = now.Add(time.Hour)
	if p.Level(b, now) != 3 || take(10) != 3 {
		t.Fatal("depois de 1 h o balde passou do teto")
	}
	if p.Refill() != 3*time.Second {
		t.Fatalf("refill = %s, esperado 3s", p.Refill())
	}
	// quanto esperar pela próxima ficha: é o Retry-After do 429
	for rate, want := range map[float64]time.Duration{1: time.Second, 4: 250 * time.Millisecond, 0.1: 10 * time.Second} {
		if got := (TokenBucket{Rate: rate}).Interval(); got != want {
			t.Errorf("Interval com RATE=%v: %s, esperado %s", rate, got, want)
		}
	}
}
