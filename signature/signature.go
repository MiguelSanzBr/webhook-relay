// Package signature é o lado de fora do relay: quem manda o webhook assina com ele, e
// quem recebe o repasse confere com ele (usando o FORWARD_SECRET). O formato é um só:
//
//	X-Webhook-Timestamp: unix em segundos
//	X-Webhook-Nonce:     único por evento, só A-Z, a-z, 0-9, _ e -, até 128
//	X-Webhook-Signature: sha256=hex(HMAC_SHA256(segredo, timestamp + "." + nonce + "." + corpo))
package signature

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

const (
	HeaderTimestamp = "X-Webhook-Timestamp"
	HeaderNonce     = "X-Webhook-Nonce"
	HeaderSignature = "X-Webhook-Signature"
)

var ErrBadNonce = errors.New("nonce inválido: só A-Z, a-z, 0-9, _ e -, até 128 caracteres")

// Sign devolve o valor do X-Webhook-Signature.
func Sign(secret []byte, ts, nonce string, body []byte) string {
	return domain.Sign(secret, ts, nonce, body)
}

// Verify confere em tempo constante. Quem recebe o repasse ainda confere a janela do
// timestamp e descarta Idempotency-Key repetida.
func Verify(secret []byte, ts, nonce string, body []byte, sig string) bool {
	return domain.Verify(secret, ts, nonce, body, sig)
}

// SetHeaders assina o corpo com o horário now e põe os três headers em h.
func SetHeaders(h http.Header, secret []byte, nonce string, body []byte, now time.Time) error {
	if !domain.ValidNonce(nonce) {
		return ErrBadNonce
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderNonce, nonce)
	h.Set(HeaderSignature, Sign(secret, ts, nonce, body))
	return nil
}
