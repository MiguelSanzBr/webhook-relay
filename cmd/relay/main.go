// relay: recebe webhook assinado, confere e repassa pro TARGET_URL.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/adapters/httpapi"
	"github.com/MiguelSanzBr/webhook-relay/internal/adapters/httpforward"
	"github.com/MiguelSanzBr/webhook-relay/internal/adapters/memory"
	"github.com/MiguelSanzBr/webhook-relay/internal/app"
	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

// exit é variável só pra o teste conseguir ver o código de saída sem matar o processo.
var exit = os.Exit

type config struct {
	addr  string
	relay *app.RelayWebhook
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("encerrando", "err", err)
		exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := load(os.Getenv)
	if err != nil {
		return err
	}
	h := &httpapi.Handler{Relay: cfg.relay, Log: log}

	srv := &http.Server{Addr: cfg.addr, Handler: h.Routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		// espera os repasses em andamento terminarem, senão o remetente toma conexão cortada
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.relay.Timeout+time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("ouvindo", "addr", cfg.addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// load lê e valida a config. Valor inválido derruba a subida, nunca cai num padrão em silêncio.
func load(getenv func(string) string) (*config, error) {
	secret := getenv("WEBHOOK_SECRET")
	if len(secret) < 32 {
		return nil, errors.New("WEBHOOK_SECRET obrigatório, com pelo menos 32 caracteres")
	}
	target := getenv("TARGET_URL")
	if target == "" {
		return nil, errors.New("TARGET_URL obrigatório")
	}

	rate, e1 := positive(getenv, "RATE", 10, func(s string) (float64, error) { return strconv.ParseFloat(s, 64) })
	burst, e2 := positive(getenv, "BURST", 20, strconv.Atoi)
	attempts, e3 := positive(getenv, "MAX_ATTEMPTS", 4, strconv.Atoi)
	timeout, e4 := positive(getenv, "TIMEOUT", 10*time.Second, time.ParseDuration)
	window, e5 := positive(getenv, "WINDOW", 5*time.Minute, time.ParseDuration)
	if err := errors.Join(e1, e2, e3, e4, e5); err != nil {
		return nil, err
	}

	addr := getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	return &config{addr: addr, relay: &app.RelayWebhook{
		Secret:    []byte(secret),
		Replay:    domain.ReplayPolicy{Window: window},
		Nonces:    memory.NewNonces(window),
		Limiter:   memory.NewBuckets(domain.TokenBucket{Rate: rate, Burst: float64(burst)}),
		Forwarder: &httpforward.Forwarder{URL: target, Client: &http.Client{}, Retry: domain.RetryPolicy{MaxAttempts: attempts, BackoffBase: 200 * time.Millisecond}},
		Clock:     time.Now,
		Timeout:   timeout,
	}}, nil
}

func positive[T int | float64 | time.Duration](getenv func(string) string, key string, def T, conv func(string) (T, error)) (T, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := conv(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s inválido: %q", key, v)
	}
	return n, nil
}
