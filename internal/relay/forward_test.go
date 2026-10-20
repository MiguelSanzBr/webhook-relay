package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func forwarder(url string) *Forwarder {
	return &Forwarder{URL: url, Client: http.DefaultClient, Retry: domain.RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}}
}

func TestForwardSendsBodyAndIdempotencyKey(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
	}))
	t.Cleanup(srv.Close)

	if err := forwarder(srv.URL).Forward(context.Background(), "n1", "application/json", []byte(`{"a":1}`)); err != nil {
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
			err := forwarder(srv.URL).Forward(context.Background(), "n", "", nil)
			if (err != nil) != c.wantErr || calls.Load() != c.wantCalls {
				t.Fatalf("err=%v chamadas=%d, esperado erro=%v chamadas=%d", err, calls.Load(), c.wantErr, c.wantCalls)
			}
		})
	}
}

func TestForwardTargetDown(t *testing.T) {
	srv := httptest.NewServer(nil)
	srv.Close()
	if err := forwarder(srv.URL).Forward(context.Background(), "n", "", nil); err == nil {
		t.Fatal("destino fora do ar não deu erro")
	}
}

func TestForwardBadURL(t *testing.T) {
	if err := forwarder("http://host com espaço/").Forward(context.Background(), "n", "", nil); err == nil {
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
	err := f.Forward(ctx, "n", "", nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v depois de %s", err, time.Since(start))
	}
}
