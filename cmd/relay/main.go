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

// maxClients é o teto de baldes do rate limit, uns 10 MB de memória.
const maxClients = 100_000

// exit é variável só pra o teste conseguir ver o código de saída sem matar o processo.
var exit = os.Exit

type config struct {
	addr       string
	relay      *app.RelayWebhook
	retryAfter time.Duration
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
	srv := cfg.server(log)
	go func() {
		<-ctx.Done()
		// espera os repasses em andamento terminarem, senão o remetente toma conexão cortada.
		// Nenhum request vive mais q o WriteTimeout, então esperar isso basta.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), srv.WriteTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("ouvindo", "addr", cfg.addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// readTimeout é o prazo pra ler header e corpo. Sem ele, mandar 1 byte a cada 2 s
// segurava a conexão pra sempre.
const readTimeout = 10 * time.Second

// server monta o http.Server com prazo pra tudo. A resposta só sai depois do repasse,
// então o WriteTimeout (q conta do fim do header) cabe a leitura e o TIMEOUT inteiro.
func (c *config) server(log *slog.Logger) *http.Server {
	h := &httpapi.Handler{Relay: c.relay, RetryAfter: c.retryAfter, Log: log}
	return &http.Server{
		Addr:              c.addr,
		Handler:           h.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      readTimeout + c.relay.Timeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// load lê e valida a config. Valor inválido derruba a subida, nunca cai num padrão em silêncio.
func load(getenv func(string) string) (*config, error) {
	secret := getenv("WEBHOOK_SECRET")
	if len(secret) < 32 {
		return nil, errors.New("WEBHOOK_SECRET obrigatório, com pelo menos 32 caracteres")
	}
	fwdSecret := getenv("FORWARD_SECRET")
	if len(fwdSecret) < 32 || fwdSecret == secret {
		return nil, errors.New("FORWARD_SECRET obrigatório, com pelo menos 32 caracteres e diferente do WEBHOOK_SECRET")
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
	limit := domain.TokenBucket{Rate: rate, Burst: float64(burst)}
	return &config{addr: addr, retryAfter: limit.Interval(), relay: &app.RelayWebhook{
		Secret:  []byte(secret),
		Replay:  domain.ReplayPolicy{Window: window},
		Nonces:  memory.NewNonces(window, time.Now),
		Limiter: memory.NewBuckets(limit, time.Now, maxClients),
		Forwarder: httpforward.New(target, []byte(fwdSecret), domain.RetryPolicy{
			MaxAttempts:    attempts,
			BackoffBase:    200 * time.Millisecond,
			AttemptTimeout: timeout / time.Duration(attempts),
		}),
		Clock:   time.Now,
		Timeout: timeout,
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
