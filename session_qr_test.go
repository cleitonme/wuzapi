package main

import (
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"go.mau.fi/whatsmeow/store"
)

func TestKillChannelCreateIfAbsent(t *testing.T) {
	const id = "qr-create-if-absent"
	defer killchannel.Delete(id)

	ch, created := killchannel.CreateIfAbsent(id)
	if !created || ch == nil {
		t.Fatal("first CreateIfAbsent should create a channel")
	}
	again, created := killchannel.CreateIfAbsent(id)
	if created {
		t.Fatal("second CreateIfAbsent created a duplicate channel")
	}
	if again != ch {
		t.Fatal("second CreateIfAbsent returned a different channel")
	}
	if got, ok := getKillChannel(id); !ok || got != ch {
		t.Fatal("getKillChannel does not return the reserved channel")
	}
}

func TestWaitForSessionClientWithoutSession(t *testing.T) {
	s := &server{}
	start := time.Now()
	if client := s.waitForSessionClient("qr-no-session", 50*time.Millisecond); client != nil {
		t.Fatal("expected nil client when no session is starting")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("returned too slowly without a session: %v", elapsed)
	}
}

func TestWaitForSessionClientWaitsWhileStarting(t *testing.T) {
	const id = "qr-wait-start"
	killchannel.Create(id)
	defer killchannel.Delete(id)

	s := &server{}
	start := time.Now()
	if client := s.waitForSessionClient(id, 250*time.Millisecond); client != nil {
		t.Fatal("expected nil client while only the kill channel exists")
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("did not wait for the starting session: %v", elapsed)
	}
}

func TestWaitForSessionClientStopsWhenSessionEnds(t *testing.T) {
	const id = "qr-wait-end"
	killchannel.Create(id)

	go func() {
		time.Sleep(100 * time.Millisecond)
		killchannel.Delete(id)
	}()

	s := &server{}
	start := time.Now()
	if client := s.waitForSessionClient(id, 5*time.Second); client != nil {
		t.Fatal("expected nil client after the session ended")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("did not stop waiting when the session ended: %v", elapsed)
	}
}

func TestStartClientCleansKillChannelOnEarlyExit(t *testing.T) {
	// Evita fetch de versão pela rede durante o teste.
	waVersionCache.Set(waVersionCacheKey, store.WAVersionContainer{2, 3000, 0}, cache.DefaultExpiration)

	const id = "qr-early-exit"
	killchannel.Create(id)

	// container é nil nos testes, então resolveDeviceStore falha antes de
	// tocar no banco e startClient retorna cedo.
	(&server{}).startClient(id, "", "", nil)

	if _, ok := getKillChannel(id); ok {
		t.Fatal("startClient left an orphan kill channel after an early return")
	}
}
