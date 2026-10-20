// sender manda um webhook assinado pro relay, do jeito q um remetente de verdade faria:
//
//	WEBHOOK_SECRET=... go run ./examples/sender '{"pedido":42}'
//
// RELAY_URL troca o endereço (padrão http://localhost:8080/webhook) e NONCE fixa o nonce,
// pra ver o replay levar 409. Sai com 1 se o relay não respondeu 2xx.
package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/signature"
)

// exit é variável só pra o teste ver o código de saída.
var exit = os.Exit

func main() {
	body := []byte(cmp.Or(strings.Join(os.Args[1:], " "), `{"pedido":42,"status":"pago"}`))
	url := cmp.Or(os.Getenv("RELAY_URL"), "http://localhost:8080/webhook")
	nonce := cmp.Or(os.Getenv("NONCE"), "evt_"+rand.Text())
	reply, err := send(url, []byte(os.Getenv("WEBHOOK_SECRET")), nonce, body)
	fmt.Println(reply)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exit(1)
	}
}

func send(url string, secret []byte, nonce string, body []byte) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := signature.SetHeaders(req.Header, secret, nonce, body, time.Now()); err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	reply := nonce + ": " + resp.Status + " " + strings.TrimSpace(string(got))
	if resp.StatusCode >= 300 {
		return reply, errors.New("relay não aceitou")
	}
	return reply, nil
}
