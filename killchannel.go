package main

import (
	"sync"

	"github.com/rs/zerolog/log"
)

type KillChannelManager struct {
	mu       sync.RWMutex
	channels map[string]chan bool
}

var killchannel = &KillChannelManager{
	channels: make(map[string]chan bool),
}

var killchannelMu sync.RWMutex

func getKillChannel(userID string) (chan bool, bool) {
	killchannelMu.RLock()
	defer killchannelMu.RUnlock()
	ch, ok := killchannel.channels[userID]
	return ch, ok
}

func deleteKillChannel(userID string, ch chan bool) {
	killchannelMu.Lock()
	if current, ok := killchannel.channels[userID]; ok && current == ch {
		delete(killchannel.channels, userID)
	}
	killchannelMu.Unlock()
}

func setKillChannel(userID string, ch chan bool) {
	killchannelMu.Lock()
	killchannel.channels[userID] = ch
	killchannelMu.Unlock()
}

func (k *KillChannelManager) Create(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.channels[id] = make(chan bool, 1)
}

func (k *KillChannelManager) Send(id string) {
	k.mu.RLock()
	ch, ok := k.channels[id]
	k.mu.RUnlock()
	if ok {
		select {
		case ch <- true:
		default:
		}
	}
}

func (k *KillChannelManager) Receive(id string) <-chan bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.channels[id]
}

func (k *KillChannelManager) Delete(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.channels, id)
}

func signalKill(userID string) {
	ch, ok := getKillChannel(userID)
	if !ok {
		log.Debug().Str("userID", userID).Msg("signalKill: no kill channel registered (already cleaned up?)")
		return
	}
	clientManager.stopSocketRecovery(userID, true)
	select {
	case ch <- true:
	default:
	}
}
