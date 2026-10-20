package relay

import (
	"sync"
	"time"
)

// RateLimiter é um token bucket por chave (o IP do cliente). Em memória, uma instância só.
type RateLimiter struct {
	rate  float64 // fichas por segundo
	burst float64
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewRateLimiter(perSecond float64, burst int, now func() time.Time) *RateLimiter {
	return &RateLimiter{rate: perSecond, burst: float64(burst), now: now, buckets: map[string]*bucket{}}
}

func (l *RateLimiter) Allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// tempo pra um balde vazio encher de novo; depois disso ele é igual a um novo e pode sair
	refill := time.Duration(l.burst / l.rate * float64(time.Second))
	if now.Sub(l.lastSweep) >= refill {
		for k, b := range l.buckets {
			if l.fill(b, now) >= l.burst {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = l.fill(b, now)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *RateLimiter) fill(b *bucket, now time.Time) float64 {
	return min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
}
