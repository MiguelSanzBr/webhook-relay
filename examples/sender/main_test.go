package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/adapters/httpapi"
	"github.com/MiguelSanzBr/webhook-relay/internal/adapters/httpforward"
	"github.com/MiguelSanzBr/webhook-relay/internal/adapters/memory"
	"github.com/MiguelSanzBr/webhook-relay/internal/app"
	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

const secret = "segredo-com-pelo-menos-32-caracteres"

// O remetente de exemplo contra o relay de verdade (handler, caso de uso e adapters reais):
// é o teste de contrato entre quem assina e quem confere.
func TestSenderAgainstRelay(t *testing.T) {
	dst := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(dst.Close)
	uc := &app.RelayWebhook{
		Secrets:   [][]byte{[]byte(secret)},
		Replay:    domain.ReplayPolicy{Window: 5 * time.Minute},
		Nonces:    memory.NewNonces(5*time.Minute, time.Now),
		Limiter:   memory.NewBuckets(domain.TokenBucket{Rate: 10, Burst: 10}, time.Now, 100),
		Forwarder: httpforward.New(dst.URL, []byte("segredo-do-relay-pro-destino"), domain.RetryPolicy{MaxAttempts: 1, AttemptTimeout: time.Second}),
		Clock:     time.Now,
		Timeout:   2 * time.Second,
	}
	relay := httptest.NewServer((&httpapi.Handler{Relay: uc, RetryAfter: time.Second, Log: slog.New(slog.DiscardHandler)}).Routes())
	t.Cleanup(relay.Close)

	args := os.Args
	t.Cleanup(func() { os.Args = args; exit = os.Exit })
	t.Setenv("WEBHOOK_SECRET", secret)
	cases := []struct {
		name, url, nonce string
		want             int
	}{
		{"primeira entrega", relay.URL + "/webhook", "evt_1", 0},
		{"mesmo nonce de novo leva 409", relay.URL + "/webhook", "evt_1", 1},
		{"sem NONCE cria um", relay.URL + "/webhook", "", 0},
		{"nonce com ponto nem sai", relay.URL + "/webhook", "evt.2", 1},
		{"relay fora", "http://127.0.0.1:1/webhook", "evt_3", 1},
		{"RELAY_URL inválida", "http://host com espaço/", "evt_4", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("RELAY_URL", c.url)
			t.Setenv("NONCE", c.nonce)
			os.Args = []string{"sender", `{"pedido":42}`}
			exited := 0
			exit = func(code int) { exited = code }
			main()
			if exited != c.want {
				t.Fatalf("exit(%d), esperado %d", exited, c.want)
			}
		})
	}
}
