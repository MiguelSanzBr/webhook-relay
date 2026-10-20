package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const maxBody = 1 << 20

// Handler junta tudo. A ordem das checagens importa: limite de taxa antes de
// ler o corpo (barato), assinatura antes do nonce (senão lixo enche a memória).
type Handler struct {
	Secret  []byte
	Guard   *ReplayGuard
	Limiter *RateLimiter
	Forward *Forwarder
	Timeout time.Duration // tempo total do repasse, retries incluídos
	Log     *slog.Logger
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", h.webhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	// atrás de proxy o RemoteAddr é o do proxy; aí precisa ler X-Forwarded-For de um proxy confiável
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !h.Limiter.Allow(ip) {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, "muitas requisições")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			writeJSON(w, http.StatusRequestEntityTooLarge, "corpo maior q 1 MiB")
			return
		}
		writeJSON(w, http.StatusBadRequest, "não deu pra ler o corpo")
		return
	}

	ts, nonce, sig := r.Header.Get("X-Webhook-Timestamp"), r.Header.Get("X-Webhook-Nonce"), r.Header.Get("X-Webhook-Signature")
	if ts == "" || nonce == "" || sig == "" {
		writeJSON(w, http.StatusUnauthorized, "faltam os headers X-Webhook-Timestamp, X-Webhook-Nonce e X-Webhook-Signature")
		return
	}
	if !Verify(h.Secret, ts, nonce, body, sig) {
		h.Log.Warn("assinatura inválida", "ip", ip)
		writeJSON(w, http.StatusUnauthorized, "assinatura inválida")
		return
	}
	if err := h.Guard.Reserve(ts, nonce); err != nil {
		code := http.StatusUnauthorized
		if errors.Is(err, ErrReplay) {
			code = http.StatusConflict
		}
		h.Log.Warn("webhook recusado", "ip", ip, "nonce", nonce, "err", err)
		writeJSON(w, code, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.Timeout)
	defer cancel()
	if err := h.Forward.Forward(ctx, nonce, r.Header.Get("Content-Type"), body); err != nil {
		h.Guard.Release(nonce)
		h.Log.Error("repasse falhou", "nonce", nonce, "err", err)
		writeJSON(w, http.StatusBadGateway, "destino não aceitou, pode tentar de novo")
		return
	}
	writeJSON(w, http.StatusOK, "repassado")
}

func writeJSON(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
