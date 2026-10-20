package signature_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/signature"
)

var secret = []byte("segredo-com-pelo-menos-32-caracteres")

// Quem manda o webhook assina assim antes de mandar pro relay.
func ExampleSetHeaders() {
	body := []byte(`{"pedido":42}`)
	req, _ := http.NewRequest(http.MethodPost, "http://localhost:8080/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if err := signature.SetHeaders(req.Header, secret, "evt_42", body, time.Unix(1760970000, 0)); err != nil {
		panic(err)
	}
	fmt.Println(req.Header.Get(signature.HeaderTimestamp))
	fmt.Println(req.Header.Get(signature.HeaderSignature))
	// Output:
	// 1760970000
	// sha256=32530d681b61836949bc4473565ce7aa647b112adeacea4a54e01c62e47a7ca8
}

func TestSetHeadersAndVerify(t *testing.T) {
	h := http.Header{}
	body := []byte(`{"a":1}`)
	if err := signature.SetHeaders(h, secret, "evt_1", body, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !signature.Verify(secret, h.Get(signature.HeaderTimestamp), h.Get(signature.HeaderNonce), body, h.Get(signature.HeaderSignature)) {
		t.Fatalf("assinatura não confere: %v", h)
	}
	// nonce com ponto o relay recusa; melhor o remetente saber na hora
	if err := signature.SetHeaders(http.Header{}, secret, "evt.1", body, time.Now()); !errors.Is(err, signature.ErrBadNonce) {
		t.Fatalf("nonce com ponto: err=%v, esperado ErrBadNonce", err)
	}
}
