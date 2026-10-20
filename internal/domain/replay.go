package domain

import (
	"errors"
	"strconv"
	"time"
)

var (
	ErrStale        = errors.New("timestamp fora da janela")
	ErrBadTimestamp = errors.New("timestamp inválido, esperado unix em segundos")
	ErrReplay       = errors.New("nonce já usado")
)

// ReplayPolicy é a regra anti-replay: timestamp dentro da janela e nonce usado uma vez só.
// Guardar o nonce fica fora do domínio, aqui só sai até quando ele precisa ficar guardado.
type ReplayPolicy struct {
	Window time.Duration
}

// Check confere a janela e devolve até quando lembrar do nonce: depois de
// ts+window o próprio timestamp já é recusado, não precisa mais dele.
func (p ReplayPolicy) Check(tsHeader string, now time.Time) (keepUntil time.Time, err error) {
	sec, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return time.Time{}, ErrBadTimestamp
	}
	ts := time.Unix(sec, 0)
	if now.Sub(ts).Abs() > p.Window {
		return time.Time{}, ErrStale
	}
	return ts.Add(p.Window), nil
}
