package main

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/types"
)

const socketRecoveryGrace = 2 * time.Second
const socketRecoveryCooldown = time.Minute

func newSocketRecovery(ctx context.Context, cancel context.CancelFunc, client recoverableSocket) *socketRecovery {
	return &socketRecovery{
		client:        client,
		ctx:           ctx,
		cancelSession: cancel,
		requests:      make(chan struct{}, 1),
		stopped:       make(chan struct{}),
	}
}

func (r *socketRecovery) stop() {
	r.stopOnce.Do(func() { close(r.stopped) })
}

func (r *socketRecovery) active() bool {
	select {
	case <-r.ctx.Done():
		return false
	case <-r.stopped:
		return false
	default:
		return true
	}
}

func (r *socketRecovery) request(err error, now time.Time) bool {
	if !errors.Is(err, socket.ErrSocketClosed) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active() || r.pending || (!r.lastAttempt.IsZero() && now.Sub(r.lastAttempt) < socketRecoveryCooldown) {
		return false
	}
	r.pending = true
	r.lastAttempt = now
	r.requests <- struct{}{}
	return true
}

func (r *socketRecovery) run(userID string, grace time.Duration) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.stopped:
			return
		case <-r.requests:
			timer := time.NewTimer(grace)
			select {
			case <-r.ctx.Done():
				timer.Stop()
				return
			case <-r.stopped:
				timer.Stop()
				return
			case <-timer.C:
			}
			r.reconnect(userID)
			r.mu.Lock()
			r.pending = false
			r.mu.Unlock()
		}
	}
}

func (r *socketRecovery) reconnect(userID string) {
	r.operation.Lock()
	defer r.operation.Unlock()
	if !r.active() {
		return
	}
	if r.client.IsConnected() {
		log.Debug().Str("userID", userID).Msg("Socket recovery skipped: connection already restored")
		return
	}
	log.Warn().Str("userID", userID).Msg("Recovering session after closed socket send error")
	err := r.client.ConnectContext(r.ctx)
	if err != nil && !errors.Is(err, whatsmeow.ErrAlreadyConnected) {
		if r.active() {
			log.Warn().Str("userID", userID).Err(err).Msg("Socket recovery connection attempt failed")
		}
		return
	}
	log.Info().Str("userID", userID).Bool("connected", r.client.IsConnected()).Msg("Socket recovery connection attempt completed")
}

func sendMessageWithRecovery(userID string, client messageSender, ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	response, err := client.SendMessage(ctx, to, message, extra...)
	clientManager.requestSocketRecovery(userID, client, err)
	return response, err
}

func sendChatPresenceWithRecovery(userID string, client chatPresenceSender, ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error {
	err := client.SendChatPresence(ctx, jid, state, media)
	clientManager.requestSocketRecovery(userID, client, err)
	return err
}