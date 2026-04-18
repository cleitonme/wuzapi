package main

import "sync"

type KillChannelManager struct {
	mu       sync.RWMutex
	channels map[string]chan bool
}

var killchannel = &KillChannelManager{
	channels: make(map[string]chan bool),
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
