package domain

import (
	"testing"
	"time"
)

func TestRetryPolicy(t *testing.T) {
	for status, want := range map[int]bool{429: true, 500: true, 503: true, 400: false, 404: false} {
		if TemporaryStatus(status) != want {
			t.Errorf("TemporaryStatus(%d) = %v", status, !want)
		}
	}

	p := RetryPolicy{MaxAttempts: 3, BackoffBase: 100 * time.Millisecond}
	if !p.Again(2, true) || p.Again(3, true) || p.Again(1, false) {
		t.Fatal("Again errado: repete só erro temporário e só até MaxAttempts")
	}

	// dobra a cada tentativa, com jitter entre metade e o valor cheio
	for attempt, full := range map[int]time.Duration{1: 100 * time.Millisecond, 3: 400 * time.Millisecond} {
		if d := p.Backoff(attempt); d < full/2 || d > full {
			t.Errorf("tentativa %d: backoff %s fora de [%s, %s]", attempt, d, full/2, full)
		}
	}
	// o shift tem teto, senão tentativa alta estoura o Duration e vira backoff negativo
	if d := p.Backoff(100); d < 0 {
		t.Fatalf("backoff negativo: %s", d)
	}
}
