package main

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	appStateFullSyncCooldown = time.Hour
	appStateRecoveryCooldown = 6 * time.Hour
	appStateRecoveryTimeout  = 2 * time.Minute
)

type appStateRecoveryAction int

const (
	appStateRecoveryNone appStateRecoveryAction = iota
	appStateRecoveryFullSync
	appStateRecoveryPeerRequest
)

type appStateRecoveryClient interface {
	FetchAppState(ctx context.Context, name appstate.WAPatchName, fullSync, onlyIfNotSynced bool) error
	SendPeerMessage(ctx context.Context, message *waE2E.Message) (whatsmeow.SendResponse, error)
}

type appStateRecovery struct {
	userID       string
	client       appStateRecoveryClient
	mu           sync.Mutex
	lastFullSync map[appstate.WAPatchName]time.Time
	lastRequest  map[appstate.WAPatchName]time.Time
}

func newAppStateRecovery(userID string, client appStateRecoveryClient) *appStateRecovery {
	return &appStateRecovery{
		userID:       userID,
		client:       client,
		lastFullSync: map[appstate.WAPatchName]time.Time{},
		lastRequest:  map[appstate.WAPatchName]time.Time{},
	}
}

func (r *appStateRecovery) plan(evt *events.AppStateSyncError, now time.Time) appStateRecoveryAction {
	if !*appStateRecoveryEnabled {
		return appStateRecoveryNone
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !evt.FullSync {
		if last, ok := r.lastFullSync[evt.Name]; ok && now.Sub(last) < appStateFullSyncCooldown {
			return appStateRecoveryNone
		}
		r.lastFullSync[evt.Name] = now
		return appStateRecoveryFullSync
	}
	if last, ok := r.lastRequest[evt.Name]; ok && now.Sub(last) < appStateRecoveryCooldown {
		return appStateRecoveryNone
	}
	r.lastRequest[evt.Name] = now
	return appStateRecoveryPeerRequest
}

func (r *appStateRecovery) handleError(ctx context.Context, evt *events.AppStateSyncError, now time.Time) {
	action := r.plan(evt, now)
	logEvent := log.Warn().Str("userID", r.userID).Str("collection", string(evt.Name)).Bool("fullSync", evt.FullSync).Err(evt.Error)
	switch action {
	case appStateRecoveryNone:
		logEvent.Bool("recoveryEnabled", *appStateRecoveryEnabled).Msg("App state sync failed; recovery skipped (disabled or cooling down)")
		return
	case appStateRecoveryFullSync:
		logEvent.Msg("App state sync failed; starting a full resync")
	case appStateRecoveryPeerRequest:
		logEvent.Msg("App state full sync failed; asking the primary device for a fresh copy")
	}

	ctx, cancel := context.WithTimeout(ctx, appStateRecoveryTimeout)
	defer cancel()
	switch action {
	case appStateRecoveryFullSync:
		if err := r.client.FetchAppState(ctx, evt.Name, true, false); err != nil {
			log.Warn().Str("userID", r.userID).Str("collection", string(evt.Name)).Err(err).Msg("App state full resync failed")
			return
		}
		log.Info().Str("userID", r.userID).Str("collection", string(evt.Name)).Msg("App state full resync completed")
	case appStateRecoveryPeerRequest:
		resp, err := r.client.SendPeerMessage(ctx, whatsmeow.BuildAppStateRecoveryRequest(evt.Name))
		if err != nil {
			log.Warn().Str("userID", r.userID).Str("collection", string(evt.Name)).Err(err).Msg("Failed to send app state recovery request")
			r.mu.Lock()
			delete(r.lastRequest, evt.Name)
			r.mu.Unlock()
			return
		}
		log.Info().Str("userID", r.userID).Str("collection", string(evt.Name)).Str("requestID", resp.ID).Msg("App state recovery requested; waiting for the primary device")
	}
}

func (r *appStateRecovery) completed(evt *events.AppStateSyncComplete) {
	if !evt.Recovery {
		return
	}
	r.mu.Lock()
	delete(r.lastFullSync, evt.Name)
	delete(r.lastRequest, evt.Name)
	r.mu.Unlock()
	log.Info().Str("userID", r.userID).Str("collection", string(evt.Name)).Uint64("version", evt.Version).Msg("App state recovered from the primary device")
}
