// Package domain tem as regras do relay sem HTTP e sem I/O: assinatura,
// janela do timestamp, token bucket e quando vale repetir o repasse.
package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	sigPrefix  = "sha256="
	nonceChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"
)

// SignedWebhook é o webhook como chegou. Timestamp, nonce e corpo são o q o
// remetente assinou; ContentType só segue junto no repasse.
type SignedWebhook struct {
	Timestamp   string
	Nonce       string
	Signature   string
	ContentType string
	Body        []byte
}

// Sign assina timestamp, nonce e corpo juntos. Quem manda o webhook calcula igual.
func Sign(secret []byte, ts, nonce string, body []byte) string {
	return sigPrefix + hex.EncodeToString(mac(secret, ts, nonce, body))
}

// ValidNonce aceita só A-Z, a-z, 0-9, _ e -, até 128. Sem ponto no nonce (e no ts,
// q o ReplayPolicy só aceita inteiro), "ts.nonce.corpo" só se divide de um jeito.
func ValidNonce(nonce string) bool {
	return nonce != "" && len(nonce) <= 128 && strings.Trim(nonce, nonceChars) == ""
}

// Verify compara em tempo constante: comparar string com == vaza pelo tempo
// de resposta quantos bytes da assinatura já estão certos. Nonce fora do formato
// é recusado antes, senão o mesmo HMAC valia com outro nonce.
func Verify(secret []byte, ts, nonce string, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, sigPrefix)
	if !ok || !ValidNonce(nonce) {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	return hmac.Equal(got, mac(secret, ts, nonce, body))
}

func mac(secret []byte, ts, nonce string, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts + "." + nonce + "."))
	m.Write(body)
	return m.Sum(nil)
}
