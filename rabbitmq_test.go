package main

import (
	"sync"
	"testing"
)

// restoreRabbitState snapshots the package-level RabbitMQ state and restores it
// when the test ends, so these tests do not leak into any other test.
func restoreRabbitState(t *testing.T) {
	t.Helper()

	rabbitMu.Lock()
	conn, channel, enabled := rabbitConn, rabbitChannel, rabbitEnabled
	declared := make(map[string]bool, len(rabbitDeclared))
	for k, v := range rabbitDeclared {
		declared[k] = v
	}
	rabbitMu.Unlock()

	t.Cleanup(func() {
		rabbitMu.Lock()
		rabbitConn, rabbitChannel, rabbitEnabled = conn, channel, enabled
		rabbitDeclared = declared
		rabbitMu.Unlock()
	})
}

// TestRabbitStateHelpers covers the accessors that replaced the bare reads and
// writes of rabbitConn/rabbitChannel/rabbitEnabled.
func TestRabbitStateHelpers(t *testing.T) {
	restoreRabbitState(t)

	setRabbitConnection(nil, nil, false)
	if rabbitIsEnabled() {
		t.Error("rabbitIsEnabled() = true after disabling, want false")
	}

	// A disabled publisher drops the Rabbit event with a sentinel error and
	// must not touch the nil channel. The webhook HTTP path is unaffected.
	if err := PublishToRabbit([]byte(`{"x":1}`)); err != ErrRabbitDisabled {
		t.Errorf("PublishToRabbit while disabled returned %v, want ErrRabbitDisabled", err)
	}

	setRabbitConnection(nil, nil, true)
	if !rabbitIsEnabled() {
		t.Error("rabbitIsEnabled() = false after enabling, want true")
	}
}

// TestRabbitConnectionSwapConcurrent reproduces the production shape: the broker
// drops the connection, handleConnectionErrors swaps rabbitConn/rabbitChannel/
// rabbitEnabled, and meanwhile event goroutines are calling PublishToRabbit.
//
// The point is the -race build. Before the fix the swap and the publishers
// touched those globals with no synchronization at all, and `go test -race`
// reports a data race here. It stays quiet once every access goes through
// rabbitMu.
//
// Note what this does NOT cover: the frame interleaving on the shared
// amqp091.Channel itself. That one happens inside the library, across a socket,
// and the race detector cannot see it — publishing is kept disabled here so the
// nil channel is never dereferenced.
func TestRabbitConnectionSwapConcurrent(t *testing.T) {
	restoreRabbitState(t)

	setRabbitConnection(nil, nil, false)

	const iterations = 500
	var wg sync.WaitGroup

	// Reconnection goroutine: the disable/enable cycle of handleConnectionErrors.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			setRabbitConnection(nil, nil, false)
		}
	}()

	// Publisher goroutines: what event dispatch does on every event while the
	// broker is down. While disabled every publish must fail
	// with the sentinel (never nil-channel deref, never silent success).
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := PublishToRabbit([]byte(`{"type":"Message"}`)); err != ErrRabbitDisabled {
					t.Errorf("PublishToRabbit returned %v, want ErrRabbitDisabled", err)
					return
				}
				_ = rabbitIsEnabled()
			}
		}()
	}

	wg.Wait()
}

// TestRabbitDeclareCacheReset ensures a connection swap invalidates the
// per-connection QueueDeclare cache (local optimization on top of PR #363).
func TestRabbitDeclareCacheReset(t *testing.T) {
	restoreRabbitState(t)

	rabbitMu.Lock()
	rabbitDeclared["q"] = true
	rabbitMu.Unlock()

	setRabbitConnection(nil, nil, false)

	rabbitMu.Lock()
	defer rabbitMu.Unlock()
	if len(rabbitDeclared) != 0 {
		t.Errorf("rabbitDeclared has %d entries after swap, want 0", len(rabbitDeclared))
	}
}
