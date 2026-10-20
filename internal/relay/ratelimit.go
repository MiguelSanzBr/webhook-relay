package relay

import (
	"sync"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

// RateLimiter é um token bucket por chave (o IP do cliente). Em memória, uma instância só.
type RateLimiter struct {
	policy domain.TokenBucket
	now    func() time.Time

	mu        sync.Mutex
	buckets   map[string]*domain.Bucket
	lastSweep time.Time
}

func NewRateLimiter(perSecond float64, burst int, now func() time.Time) *RateLimiter {
	return &RateLimiter{policy: domain.TokenBucket{Rate: perSecond, Burst: float64(burst)}, now: now, buckets: map[string]*domain.Bucket{}}
}

func (l *RateLimiter) Allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) >= l.policy.Refill() {
		for k, b := range l.buckets {
			if l.policy.Level(*b, now) >= l.policy.Burst {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b, ok := l.buckets[key]
	if !ok {
		nb := l.policy.New(now)
		b = &nb
		l.buckets[key] = b
	}
	return l.policy.Take(b, now)
}
