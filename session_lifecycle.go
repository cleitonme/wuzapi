package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

var errSessionActive = errors.New("session is active; disconnect and wait for shutdown first")

var deletingSessionsMu sync.Mutex
var deletingSessions = make(map[string]bool)

func reserveSessionDeletion(userID string) error {
	deletingSessionsMu.Lock()
	defer deletingSessionsMu.Unlock()
	if deletingSessions[userID] {
		return errors.New("user deletion is already in progress")
	}
	deletingSessions[userID] = true
	return nil
}

func releaseSessionDeletion(userID string) {
	deletingSessionsMu.Lock()
	delete(deletingSessions, userID)
	deletingSessionsMu.Unlock()
}

func sessionContext(kill <-chan bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-kill:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func sessionActive(userID string) bool {
	if _, registered := getKillChannel(userID); registered {
		return true
	}
	client := clientManager.GetWhatsmeowClient(userID)
	return client != nil && client.IsConnected()
}

func (s *server) clearSessionQRCode(userID string) error {
	_, err := s.db.Exec(`UPDATE users SET qrcode='' WHERE id=$1`, userID)
	return err
}

func connectSessionWithRetry(ctx context.Context, connect func() error, attempts int, baseWait time.Duration) error {
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			waitTime := time.Duration(attempt) * baseWait
			log.Warn().
				Int("attempt", attempt+1).
				Int("max_retries", attempts).
				Dur("wait_time", waitTime).
				Msg("Retrying connection after delay")
			timer := time.NewTimer(waitTime)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}

		err := connect()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			log.Info().
				Int("attempt", attempt+1).
				Msg("Successfully connected to WhatsApp")
			return nil
		}

		lastErr = err
		log.Warn().
			Err(err).
			Int("attempt", attempt+1).
			Int("max_retries", attempts).
			Msg("Failed to connect to WhatsApp")
	}

	return lastErr
}

func waitForSessionStop(ctx context.Context, userID string) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, running := getKillChannel(userID); !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type sessionDeletionClient interface {
	IsConnected() bool
	Logout(context.Context) error
	Disconnect()
}

func disconnectSessionForDeletion(ctx context.Context, client sessionDeletionClient, preserveDevices bool) error {
	var err error
	if !preserveDevices && client.IsConnected() {
		err = client.Logout(ctx)
	}
	client.Disconnect()
	return err
}

func (s *server) setDisconnectedState(txtid string, clearEvents bool) error {
	if clearEvents {
		_, err := s.db.Exec("UPDATE users SET connected=0,events=$1 WHERE id=$2", "", txtid)
		return err
	}
	_, err := s.db.Exec("UPDATE users SET connected=0 WHERE id=$1", txtid)
	return err
}

func resolveConnectEvents(subscribe []string, existing string) (eventstring string, changed bool) {
	if len(subscribe) < 1 {
		return existing, false
	}
	var subscribedEvents []string
	for _, arg := range subscribe {
		if !Find(supportedEventTypes, arg) {
			log.Warn().Str("Type", arg).Msg("Event type discarded")
			continue
		}
		if !Find(subscribedEvents, arg) {
			subscribedEvents = append(subscribedEvents, arg)
		}
	}
	return strings.Join(subscribedEvents, ","), true
}