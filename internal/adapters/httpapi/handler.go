// Package httpapi é a porta de entrada HTTP: lê o webhook, chama o caso de uso
// e traduz o erro dele pra status.
package httpapi

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"expvar"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/app"
	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

const maxBody = 1 << 20

var errMissingHeaders = errors.New("faltam os headers X-Webhook-Timestamp, X-Webhook-Nonce e X-Webhook-Signature")

// Handler traduz HTTP pro caso de uso e o erro dele de volta pra status.
type Handler struct {
	Relay      *app.RelayWebhook
	RetryAfter time.Duration // quanto o cliente com 429 espera, vem do RATE
	Log        *slog.Logger
}

// statuses conta as respostas por rota e status, ex. "POST /webhook 401". Sai em /debug/vars.
var statuses = expvar.NewMap("http_status")

type logKey struct{}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", h.webhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.Handle("GET /debug/vars", expvar.Handler())
	return h.observe(mux)
}

// observe dá um id pra cada request, põe ele em todo log do request, loga uma linha no fim
// com status e duração e conta o status. O id de fora só vale no formato do nonce: curto e limpo.
func (h *Handler) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-Id")
		if !domain.ValidNonce(id) {
			id = rand.Text()
		}
		w.Header().Set("X-Request-Id", id)
		log := h.Log.With("req_id", id)
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		r = r.WithContext(context.WithValue(r.Context(), logKey{}, log))

		next.ServeHTTP(sw, r)

		route := cmp.Or(r.Pattern, "sem rota") // o mux preenche o Pattern no r q recebeu
		statuses.Add(route+" "+strconv.Itoa(sw.code), 1)
		if route != "GET /healthz" { // healthcheck a cada 30 s só enche o log
			log.Info("request", "route", route, "status", sw.code, "dur_ms", time.Since(start).Milliseconds())
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(logKey{}).(*slog.Logger)
	// atrás de proxy o RemoteAddr é o do proxy; aí precisa ler X-Forwarded-For de um proxy confiável
	ip := clientKey(r.RemoteAddr)
	var hook domain.SignedWebhook
	err := h.Relay.Relay(r.Context(), ip, func() (domain.SignedWebhook, error) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			return hook, err
		}
		hook = domain.SignedWebhook{
			Timestamp:   r.Header.Get("X-Webhook-Timestamp"),
			Nonce:       r.Header.Get("X-Webhook-Nonce"),
			Signature:   r.Header.Get("X-Webhook-Signature"),
			ContentType: r.Header.Get("Content-Type"),
			Body:        body,
		}
		if hook.Timestamp == "" || hook.Nonce == "" || hook.Signature == "" {
			return hook, errMissingHeaders
		}
		return hook, nil
	})

	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, "repassado")
	case errors.Is(err, app.ErrUnavailable):
		log.Error("store fora", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, "indisponível, tente de novo")
	case errors.Is(err, app.ErrRateLimited):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(h.RetryAfter.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, "muitas requisições")
	case errors.As(err, new(*http.MaxBytesError)):
		// o statusWriter esconde do MaxBytesReader o aviso pra fechar a conexão, então fecha aqui
		w.Header().Set("Connection", "close")
		writeJSON(w, http.StatusRequestEntityTooLarge, "corpo maior q 1 MiB")
	case errors.Is(err, errMissingHeaders):
		writeJSON(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, app.ErrBadSignature):
		log.Warn("assinatura inválida", "ip", ip)
		writeJSON(w, http.StatusUnauthorized, "assinatura inválida")
	case errors.Is(err, domain.ErrStale), errors.Is(err, domain.ErrBadTimestamp), errors.Is(err, domain.ErrReplay):
		code := http.StatusUnauthorized
		if errors.Is(err, domain.ErrReplay) {
			code = http.StatusConflict
		}
		log.Warn("webhook recusado", "ip", ip, "nonce", hook.Nonce, "err", err)
		writeJSON(w, code, err.Error())
	case errors.As(err, new(*app.ForwardError)):
		log.Error("repasse falhou", "nonce", hook.Nonce, "err", err)
		writeJSON(w, http.StatusBadGateway, "destino não aceitou, pode tentar de novo")
	default: // sobra só erro lendo o corpo
		writeJSON(w, http.StatusBadRequest, "não deu pra ler o corpo")
	}
}

// clientKey é a chave do rate limit. IPv6 vale pelo /64, q é o q qualquer VPS recebe:
// por endereço, quem tem um /64 trocava de IP a cada requisição e não tinha limite.
func clientKey(remoteAddr string) string {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	ip := ap.Addr().Unmap()
	if ip.Is4() {
		return ip.String()
	}
	p, _ := ip.Prefix(64) // só falha com bits fora de 0..128
	return p.String()
}

func writeJSON(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
