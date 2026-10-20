// Package httpapi é a porta de entrada HTTP: lê o webhook, chama o caso de uso
// e traduz o erro dele pra status.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"

	"github.com/MiguelSanzBr/webhook-relay/internal/app"
	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

const maxBody = 1 << 20

var errMissingHeaders = errors.New("faltam os headers X-Webhook-Timestamp, X-Webhook-Nonce e X-Webhook-Signature")

// Handler traduz HTTP pro caso de uso e o erro dele de volta pra status.
type Handler struct {
	Relay *app.RelayWebhook
	Log   *slog.Logger
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", h.webhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
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
		h.Log.Error("store fora", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, "indisponível, tente de novo")
	case errors.Is(err, app.ErrRateLimited):
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, "muitas requisições")
	case errors.As(err, new(*http.MaxBytesError)):
		writeJSON(w, http.StatusRequestEntityTooLarge, "corpo maior q 1 MiB")
	case errors.Is(err, errMissingHeaders):
		writeJSON(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, app.ErrBadSignature):
		h.Log.Warn("assinatura inválida", "ip", ip)
		writeJSON(w, http.StatusUnauthorized, "assinatura inválida")
	case errors.Is(err, domain.ErrStale), errors.Is(err, domain.ErrBadTimestamp), errors.Is(err, domain.ErrReplay):
		code := http.StatusUnauthorized
		if errors.Is(err, domain.ErrReplay) {
			code = http.StatusConflict
		}
		h.Log.Warn("webhook recusado", "ip", ip, "nonce", hook.Nonce, "err", err)
		writeJSON(w, code, err.Error())
	case errors.As(err, new(*app.ForwardError)):
		h.Log.Error("repasse falhou", "nonce", hook.Nonce, "err", err)
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
