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

// getKillChannel, deleteKillChannel, setKillChannel e os métodos do
// KillChannelManager compartilham o mesmo mapa e o mesmo mutex para evitar
// corrida entre o caminho de conexão e o de encerramento da sessão.
func getKillChannel(userID string) (chan bool, bool) {
	killchannel.mu.RLock()
	defer killchannel.mu.RUnlock()
	ch, ok := killchannel.channels[userID]
	return ch, ok
}

func deleteKillChannel(userID string, ch chan bool) {
	killchannel.mu.Lock()
	if current, ok := killchannel.channels[userID]; ok && current == ch {
		delete(killchannel.channels, userID)
	}
	killchannel.mu.Unlock()
}

func setKillChannel(userID string, ch chan bool) {
	killchannel.mu.Lock()
	killchannel.channels[userID] = ch
	killchannel.mu.Unlock()
}

func (k *KillChannelManager) Create(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.channels[id] = make(chan bool, 1)
}

// CreateIfAbsent registra um canal apenas se ainda não houver sessão
// registrada para o usuário. Retorna o canal e se ele foi criado agora.
func (k *KillChannelManager) CreateIfAbsent(id string) (chan bool, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if ch, ok := k.channels[id]; ok {
		return ch, false
	}
	ch := make(chan bool, 1)
	k.channels[id] = ch
	return ch, true
}

// Ensure devolve o canal já registrado ou cria um novo. Usado no início de
// startClient para capturar o canal da sessão antes de qualquer retorno.
func (k *KillChannelManager) Ensure(id string) chan bool {
	ch, _ := k.CreateIfAbsent(id)
	return ch
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
