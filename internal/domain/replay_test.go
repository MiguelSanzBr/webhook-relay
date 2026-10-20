package domain

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestReplayWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := ReplayPolicy{Window: 5 * time.Minute}

	keep, err := p.Check(unix(now), now)
	if err != nil || !keep.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("na janela: keepUntil=%s err=%v", keep, err)
	}

	cases := []struct{ name, ts string }{
		{"6 min no passado", unix(now.Add(-6 * time.Minute))},
		{"6 min no futuro", unix(now.Add(6 * time.Minute))},
	}
	for _, c := range cases {
		if _, err := p.Check(c.ts, now); !errors.Is(err, ErrStale) {
			t.Errorf("%s: err=%v, esperado ErrStale", c.name, err)
		}
	}
	// lixo no header não é "velho", é inválido: no log aparece a diferença
	for _, ts := range []string{"ontem", "1.5", ""} {
		if _, err := p.Check(ts, now); !errors.Is(err, ErrBadTimestamp) {
			t.Errorf("ts %q: err=%v, esperado ErrBadTimestamp", ts, err)
		}
	}
}
