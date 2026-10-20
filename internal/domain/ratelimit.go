package domain

import "time"

// TokenBucket é a conta do rate limit: Rate fichas por segundo, até Burst guardadas.
// O relógio entra por parâmetro, então não tem sleep nem time.Now aqui.
type TokenBucket struct {
	Rate  float64
	Burst float64
}

// Bucket é o balde de um cliente.
type Bucket struct {
	Tokens float64
	Last   time.Time
}

func (p TokenBucket) New(now time.Time) Bucket {
	return Bucket{Tokens: p.Burst, Last: now}
}

// Take repõe o q passou desde a última vez e gasta uma ficha, se tiver.
func (p TokenBucket) Take(b *Bucket, now time.Time) bool {
	b.Tokens = p.Level(*b, now)
	b.Last = now
	if b.Tokens < 1 {
		return false
	}
	b.Tokens--
	return true
}

// Level é quantas fichas o balde teria agora. Teto na rajada, parado não acumula.
func (p TokenBucket) Level(b Bucket, now time.Time) float64 {
	return min(p.Burst, b.Tokens+now.Sub(b.Last).Seconds()*p.Rate)
}

// Refill é o tempo pra um balde vazio encher de novo; depois disso ele é igual a um novo.
func (p TokenBucket) Refill() time.Duration {
	return time.Duration(p.Burst / p.Rate * float64(time.Second))
}
