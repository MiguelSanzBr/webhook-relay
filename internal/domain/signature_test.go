package domain

import (
	"strings"
	"testing"
)

func TestVerify(t *testing.T) {
	secret := []byte("s3gredo")
	body := []byte(`{"pedido":42}`)
	good := Sign(secret, "1700000000", "n1", body)
	long := strings.Repeat("a", 129)

	cases := []struct {
		name       string
		secret     []byte
		ts, nonce  string
		body       []byte
		header     string
		wantResult bool
	}{
		{"assinatura certa", secret, "1700000000", "n1", body, good, true},
		{"corpo alterado", secret, "1700000000", "n1", []byte(`{"pedido":43}`), good, false},
		// timestamp e nonce entram no HMAC, senão dava pra reaproveitar a assinatura com outro nonce
		{"timestamp alterado", secret, "1700000001", "n1", body, good, false},
		{"nonce alterado", secret, "1700000000", "n2", body, good, false},
		{"segredo errado", []byte("outro"), "1700000000", "n1", body, good, false},
		{"sem prefixo sha256=", secret, "1700000000", "n1", body, good[len("sha256="):], false},
		{"hex inválido", secret, "1700000000", "n1", body, "sha256=zz", false},
		{"header vazio", secret, "1700000000", "n1", body, "", false},
		// "ts.nonce.corpo" com ponto no nonce é ambíguo: dava pra mover o começo do corpo
		// pro nonce, a mesma assinatura valia e o anti-replay via outro nonce
		{"nonce deslocado pelo ponto", secret, "1700000000", `evt_1.{"v":10`, []byte(`5}`), Sign(secret, "1700000000", "evt_1", []byte(`{"v":10.5}`)), false},
		{"nonce com _ e -", secret, "1700000000", "evt_1-a", body, Sign(secret, "1700000000", "evt_1-a", body), true},
		{"nonce com espaço", secret, "1700000000", "n 1", body, Sign(secret, "1700000000", "n 1", body), false},
		{"nonce com 129 caracteres", secret, "1700000000", long, body, Sign(secret, "1700000000", long, body), false},
		{"nonce com 128 caracteres", secret, "1700000000", long[1:], body, Sign(secret, "1700000000", long[1:], body), true},
	}
	for _, c := range cases {
		if got := Verify(c.secret, c.ts, c.nonce, c.body, c.header); got != c.wantResult {
			t.Errorf("%s: Verify = %v, esperado %v", c.name, got, c.wantResult)
		}
	}
}
