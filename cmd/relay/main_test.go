package main

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/adapters/httpforward"
	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

const (
	goodSecret = "um-segredo-de-pelo-menos-32-caracteres"
	fwdSecret  = "outro-segredo-so-do-relay-pro-destino"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfig(t *testing.T) {
	base := map[string]string{"WEBHOOK_SECRET": goodSecret, "FORWARD_SECRET": fwdSecret, "TARGET_URL": "http://destino/hook"}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}

	cfg, err := load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	retry := cfg.relay.Forwarder.(*httpforward.Forwarder).Retry
	if cfg.addr != ":8080" || cfg.relay.Timeout != 10*time.Second || retry.MaxAttempts != 4 {
		t.Fatalf("padrões errados: addr=%s timeout=%s attempts=%d", cfg.addr, cfg.relay.Timeout, retry.MaxAttempts)
	}
	// destino travado não pode gastar o TIMEOUT todo na 1ª tentativa
	if retry.AttemptTimeout != 2500*time.Millisecond || retry.BackoffBase <= 0 {
		t.Fatalf("prazo por tentativa %s, esperado TIMEOUT/MAX_ATTEMPTS = 2.5s; backoff %s", retry.AttemptTimeout, retry.BackoffBase)
	}

	// rotação: WEBHOOK_SECRET com dois segredos separados por vírgula, os dois valem
	rotating, err := load(env(with("WEBHOOK_SECRET", goodSecret+",segredo-novo-tambem-com-32-caracteres")))
	if err != nil || len(rotating.relay.Secrets) != 2 {
		t.Fatalf("rotação: %v err=%v", rotating, err)
	}

	custom, err := load(env(with("TIMEOUT", "3s")))
	if err != nil || custom.relay.Timeout != 3*time.Second {
		t.Fatalf("TIMEOUT=3s ignorado: %v err=%v", custom, err)
	}

	// config ruim tem q derrubar na subida, não virar um relay q aceita qualquer coisa
	bad := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"sem segredo", with("WEBHOOK_SECRET", ""), "WEBHOOK_SECRET"},
		{"segredo curto", with("WEBHOOK_SECRET", "123"), "WEBHOOK_SECRET"},
		{"sem destino", with("TARGET_URL", ""), "TARGET_URL"},
		// URL ruim subia e todo repasse falhava depois
		{"destino sem esquema", with("TARGET_URL", "destino"), "TARGET_URL"},
		{"destino ftp", with("TARGET_URL", "ftp://destino/"), "TARGET_URL"},
		{"destino sem host", with("TARGET_URL", "http:///hook"), "TARGET_URL"},
		{"segundo segredo curto", with("WEBHOOK_SECRET", goodSecret+",123"), "WEBHOOK_SECRET"},
		{"segredo do repasse igual a um dos de entrada", with("WEBHOOK_SECRET", "segredo-novo-tambem-com-32-caracteres,"+fwdSecret), "FORWARD_SECRET"},
		{"sem segredo do repasse", with("FORWARD_SECRET", ""), "FORWARD_SECRET"},
		{"segredo do repasse curto", with("FORWARD_SECRET", "123"), "FORWARD_SECRET"},
		// igual ao de entrada, o webhook original já passava no destino sem o relay
		{"segredo do repasse igual ao de entrada", with("FORWARD_SECRET", goodSecret), "FORWARD_SECRET"},
		{"RATE não numérico", with("RATE", "muito"), "RATE"},
		{"RATE zero", with("RATE", "0"), "RATE"},
		{"BURST inválido", with("BURST", "-1"), "BURST"},
		{"MAX_ATTEMPTS inválido", with("MAX_ATTEMPTS", "x"), "MAX_ATTEMPTS"},
		{"TIMEOUT inválido", with("TIMEOUT", "10"), "TIMEOUT"},
		{"WINDOW inválido", with("WINDOW", "-5m"), "WINDOW"},
	}
	for _, c := range bad {
		if _, err := load(env(c.env)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v, esperado erro citando %s", c.name, err, c.want)
		}
	}
}

// Sem prazo de leitura e de ociosidade, 1 byte a cada 2 s segurava a conexão pra sempre
// (slowloris) e keep-alive parado nunca fechava. Cada prazo tem q existir, e o de escrita
// tem q caber a leitura mais o repasse inteiro, senão corta repasse bom no meio.
func TestServerHasDeadlines(t *testing.T) {
	cfg, err := load(env(map[string]string{"WEBHOOK_SECRET": goodSecret, "FORWARD_SECRET": fwdSecret, "TARGET_URL": "http://destino/", "TIMEOUT": "7s"}))
	if err != nil {
		t.Fatal(err)
	}
	srv := cfg.server(slog.New(slog.DiscardHandler))
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("prazo zerado: header=%s leitura=%s ocioso=%s", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes <= 0 || srv.MaxHeaderBytes > 64<<10 {
		t.Fatalf("MaxHeaderBytes=%d, esperado um teto pequeno", srv.MaxHeaderBytes)
	}
	if srv.WriteTimeout < srv.ReadTimeout+7*time.Second {
		t.Fatalf("WriteTimeout=%s não cabe leitura (%s) + TIMEOUT (7s)", srv.WriteTimeout, srv.ReadTimeout)
	}
}

// A montagem do main ligada de verdade, pelo handler: BURST=2, RATE=0.001 e WINDOW=10m.
// Fresco passa, 6 min de idade passa (com a janela padrão de 5 min seria 401) e o terceiro
// leva 429 com Retry-After de 1000 s. RATE e BURST trocados ou WINDOW no lugar errado quebram.
func TestWiringFromEnv(t *testing.T) {
	dst := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(dst.Close)
	cfg, err := load(env(map[string]string{
		"WEBHOOK_SECRET": goodSecret, "FORWARD_SECRET": fwdSecret, "TARGET_URL": dst.URL,
		"RATE": "0.001", "BURST": "2", "WINDOW": "10m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.server(slog.New(slog.DiscardHandler)).Handler
	send := func(nonce string, age time.Duration) *httptest.ResponseRecorder {
		ts := strconv.FormatInt(time.Now().Add(-age).Unix(), 10)
		r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`))
		r.Header.Set("X-Webhook-Timestamp", ts)
		r.Header.Set("X-Webhook-Nonce", nonce)
		r.Header.Set("X-Webhook-Signature", domain.Sign([]byte(goodSecret), ts, nonce, []byte(`{}`)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	if got := send("n1", 0).Code; got != http.StatusOK {
		t.Fatalf("primeiro: HTTP %d, esperado 200", got)
	}
	if got := send("n2", 6*time.Minute).Code; got != http.StatusOK {
		t.Fatalf("6 min com WINDOW=10m: HTTP %d, esperado 200", got)
	}
	third := send("n3", 0)
	if third.Code != http.StatusTooManyRequests || third.Header().Get("Retry-After") != "1000" {
		t.Fatalf("terceiro: HTTP %d Retry-After=%q, esperado 429 e 1000", third.Code, third.Header().Get("Retry-After"))
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// SIGINT de verdade no próprio processo: main tem q desligar limpo, sem exit(1).
func TestMainGracefulShutdown(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("ADDR", addr)
	t.Setenv("WEBHOOK_SECRET", goodSecret)
	t.Setenv("FORWARD_SECRET", fwdSecret)
	t.Setenv("TARGET_URL", "http://127.0.0.1:1/")
	exited := -1
	exit = func(code int) { exited = code }
	t.Cleanup(func() { exit = os.Exit })

	done := make(chan struct{})
	go func() { main(); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay não subiu: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("main não terminou depois do SIGINT")
	}
	if exited != -1 {
		t.Fatalf("exit(%d) chamado num shutdown limpo", exited)
	}
}

func TestMainExitsWithOneOnError(t *testing.T) {
	cases := map[string]map[string]string{
		"config ruim":    {"WEBHOOK_SECRET": ""},
		"porta inválida": {"WEBHOOK_SECRET": goodSecret, "FORWARD_SECRET": fwdSecret, "TARGET_URL": "http://x/", "ADDR": "porta-que-nao-existe"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			for _, k := range []string{"WEBHOOK_SECRET", "FORWARD_SECRET", "TARGET_URL", "ADDR"} {
				t.Setenv(k, vars[k])
			}
			exited := -1
			exit = func(code int) { exited = code }
			t.Cleanup(func() { exit = os.Exit })

			main()
			if exited != 1 {
				t.Fatalf("exit = %d, esperado 1", exited)
			}
		})
	}
}

// distroless não tem shell nem curl: o próprio binário faz o healthcheck do container
// com `relay -health`, batendo no /healthz da porta do ADDR.
func TestHealthFlag(t *testing.T) {
	server := func(code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(srv.Close)
		_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
		return ":" + port
	}
	cases := []struct {
		name, addr string
		want       int
	}{
		{"no ar", server(http.StatusNoContent), 0},
		{"respondendo erro", server(http.StatusServiceUnavailable), 1},
		{"porta fechada", freeAddr(t), 1},
		{"ADDR sem porta", "sem-porta", 1},
	}
	args := os.Args
	t.Cleanup(func() { os.Args = args; exit = os.Exit })
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ADDR", c.addr)
			os.Args = []string{"relay", "-health"}
			exited := -1
			exit = func(code int) { exited = code }
			main()
			if exited != c.want {
				t.Fatalf("exit(%d), esperado %d", exited, c.want)
			}
		})
	}
}
