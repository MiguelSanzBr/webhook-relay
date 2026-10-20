package httpforward

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MiguelSanzBr/webhook-relay/internal/domain"
)

// target responde com os status da lista, um por chamada, e repete o último.
func target(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(calls.Add(1)) - 1
		w.WriteHeader(statuses[min(n, len(statuses)-1)])
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

var hook = domain.SignedWebhook{Nonce: "n"}

func forwarder(url string) *Forwarder {
	return New(url, domain.RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond, AttemptTimeout: 5 * time.Second})
}

func TestForwardSendsBodyAndIdempotencyKey(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
	}))
	t.Cleanup(srv.Close)

	if err := forwarder(srv.URL).Forward(context.Background(), domain.SignedWebhook{Nonce: "n1", ContentType: "application/json", Body: []byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	// o nonce vira Idempotency-Key: se o relay reenviar, o destino descarta a cópia
	if got.Header.Get("Idempotency-Key") != "n1" || got.Header.Get("Content-Type") != "application/json" || string(gotBody) != `{"a":1}` {
		t.Fatalf("repasse chegou errado: headers=%v body=%s", got.Header, gotBody)
	}
}

func TestForwardRetries(t *testing.T) {
	cases := []struct {
		name      string
		statuses  []int
		wantCalls int32
		wantErr   bool
	}{
		{"503 duas vezes e depois 200", []int{503, 503, 200}, 3, false},
		{"429 conta como temporário", []int{429, 202}, 2, false},
		{"400 não adianta repetir", []int{400}, 1, true},
		{"500 até acabar as tentativas", []int{500}, 3, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, calls := target(t, c.statuses...)
			err := forwarder(srv.URL).Forward(context.Background(), hook)
			if (err != nil) != c.wantErr || calls.Load() != c.wantCalls {
				t.Fatalf("err=%v chamadas=%d, esperado erro=%v chamadas=%d", err, calls.Load(), c.wantErr, c.wantCalls)
			}
		})
	}
}

func TestForwardTargetDown(t *testing.T) {
	srv := httptest.NewServer(nil)
	srv.Close()
	if err := forwarder(srv.URL).Forward(context.Background(), hook); err == nil {
		t.Fatal("destino fora do ar não deu erro")
	}
}

func TestForwardBadURL(t *testing.T) {
	if err := forwarder("http://host com espaço/").Forward(context.Background(), hook); err == nil {
		t.Fatal("URL inválida não deu erro")
	}
}

// Cliente desistiu no meio do backoff: o relay para na hora em vez de dormir o backoff inteiro.
func TestForwardStopsWhenContextEnds(t *testing.T) {
	srv, _ := target(t, 503)
	f := forwarder(srv.URL)
	f.Retry.BackoffBase = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := f.Forward(ctx, hook)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v depois de %s", err, time.Since(start))
	}
}

// Destino responde redirect: o relay não segue. Seguindo, um 307 levava o corpo pra outro host
// (SSRF a partir do destino) e um 302 virava GET sem corpo, contado como entregue.
func TestForwardDoesNotFollowRedirects(t *testing.T) {
	other, otherCalls := target(t, http.StatusOK)
	for _, code := range []int{301, 302, 307, 308} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Redirect(w, r, other.URL, code)
			}))
			t.Cleanup(srv.Close)

			if err := forwarder(srv.URL).Forward(context.Background(), hook); err == nil {
				t.Fatal("redirect contado como entregue")
			}
			// 3xx não é temporário: uma chamada só, e o outro host nunca é tocado
			if calls.Load() != 1 || otherCalls.Load() != 0 {
				t.Fatalf("destino=%d outro host=%d, esperado 1 e 0", calls.Load(), otherCalls.Load())
			}
		})
	}
}

// Destino aceita a conexão e trava: cada tentativa tem prazo próprio, então o retry acontece
// em vez de a primeira tentativa gastar o TIMEOUT inteiro.
func TestForwardRetriesHungTarget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done() // trava até o relay desistir da tentativa
		}
	}))
	t.Cleanup(srv.Close)
	f := forwarder(srv.URL)
	f.Retry.AttemptTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := f.Forward(ctx, hook); err != nil || calls.Load() != 2 {
		t.Fatalf("err=%v chamadas=%d, esperado sucesso na 2ª", err, calls.Load())
	}
}
