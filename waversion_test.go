package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.mau.fi/whatsmeow/store"
)

func TestParseWAVersion(t *testing.T) {
	v, err := parseWAVersion([]byte(`[2, 3000, 1048620361]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != (store.WAVersionContainer{2, 3000, 1048620361}) {
		t.Fatalf("unexpected version: %s", v.String())
	}
	if _, err := parseWAVersion([]byte(`[2, 3000]`)); err == nil {
		t.Fatal("expected error for 2 parts")
	}
	if _, err := parseWAVersion([]byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid json")
	}
}

func TestGreaterWAVersion(t *testing.T) {
	a := store.WAVersionContainer{2, 3000, 1049558099}
	b := store.WAVersionContainer{2, 3000, 1048620361}
	if greaterWAVersion(a, b) != a {
		t.Fatal("expected a (maior)")
	}
	if greaterWAVersion(b, a) != a {
		t.Fatal("expected a (maior) na ordem inversa")
	}
	if greaterWAVersion(a, a) != a {
		t.Fatal("empate deve retornar a")
	}
}

func TestResolveWAVersionUsesCache(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = w.Write([]byte(`[2, 3000, 1049999999]`))
	}))
	defer srv.Close()

	oldURLs := waVersionURLs
	oldFallback := fallbackWAVersion
	defer func() {
		waVersionURLs = oldURLs
		fallbackWAVersion = oldFallback
		waVersionCache.Flush()
	}()

	waVersionURLs = []string{srv.URL}
	fallbackWAVersion = store.WAVersionContainer{2, 3000, 1}
	waVersionCache.Flush()

	first := ResolveWAVersion(context.Background())
	if first != (store.WAVersionContainer{2, 3000, 1049999999}) {
		t.Fatalf("expected remote version, got %s", first.String())
	}
	second := ResolveWAVersion(context.Background())
	if second != first {
		t.Fatalf("expected cached version, got %s", second.String())
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("expected 1 HTTP fetch (cache), got %d", got)
	}
}

func TestResolveWAVersionFallbackOnError(t *testing.T) {
	oldURLs := waVersionURLs
	oldFallback := fallbackWAVersion
	defer func() {
		waVersionURLs = oldURLs
		fallbackWAVersion = oldFallback
		waVersionCache.Flush()
	}()

	waVersionURLs = []string{"http://127.0.0.1:1/unreachable"}
	fallbackWAVersion = store.WAVersionContainer{2, 3000, 1049558099}
	waVersionCache.Flush()

	got := ResolveWAVersion(context.Background())
	if got != fallbackWAVersion {
		t.Fatalf("expected fallback, got %s", got.String())
	}
}
