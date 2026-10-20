// Package domain tem as regras do relay sem HTTP e sem I/O: assinatura,
// janela do timestamp, token bucket e quando vale repetir o repasse.
package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const sigPrefix = "sha256="

// Sign assina timestamp, nonce e corpo juntos. Quem manda o webhook calcula igual.
func Sign(secret []byte, ts, nonce string, body []byte) string {
	return sigPrefix + hex.EncodeToString(mac(secret, ts, nonce, body))
}

// Verify compara em tempo constante: comparar string com == vaza pelo tempo
// de resposta quantos bytes da assinatura já estão certos.
func Verify(secret []byte, ts, nonce string, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, sigPrefix)
	if !ok {
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
