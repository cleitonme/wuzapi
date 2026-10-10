package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow/store"
)

// Fontes remotas na mesma ordem do exemplo Baileys (wbot.ts).
// Primeira válida vence; se todas falharem, usa-se fallbackWAVersion (main.go).
var waVersionURLs = []string{
	"https://raw.githubusercontent.com/cleitonme/Whazing-SaaS/refs/heads/main/waversion.json",
	"https://scripts.infomeurer.com.br/waversion.json",
}

const waVersionCacheKey = "waVersion"

var (
	// TTL 24h + cleanup 30min: equivalente ao NodeCache {stdTTL: 24h} do exemplo.
	// Garante que o download não acontece a cada conexão, só no miss/expiração.
	waVersionCache = cache.New(24*time.Hour, 30*time.Minute)
	// Serializa get+fetch+set entre connects concorrentes (boot com N sessões).
	waVersionMu sync.Mutex
)

func parseWAVersion(data []byte) (store.WAVersionContainer, error) {
	var parts []uint32
	if err := json.Unmarshal(data, &parts); err != nil {
		return store.WAVersionContainer{}, fmt.Errorf("invalid waversion.json: %w", err)
	}
	if len(parts) != 3 {
		return store.WAVersionContainer{}, fmt.Errorf("invalid waversion.json: expected 3 parts, got %d", len(parts))
	}
	return store.WAVersionContainer{parts[0], parts[1], parts[2]}, nil
}

// greaterWAVersion retorna a maior entre a e b (empate retorna a),
// equivalente ao getGreaterVersion do exemplo Baileys.
func greaterWAVersion(a, b store.WAVersionContainer) store.WAVersionContainer {
	if a.LessThan(b) {
		return b
	}
	return a
}

func fetchProjectWAVersion(ctx context.Context) *store.WAVersionContainer {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	for _, url := range waVersionURLs {
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			log.Warn().Err(err).Str("url", url).Msg("Failed to prepare WA version request")
			continue
		}
		resp, err := httpClient.Do(req)
		cancel()
		if err != nil {
			log.Warn().Err(err).Str("url", url).Msg("Connection error fetching WA version")
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64))
		status := resp.StatusCode
		_ = resp.Body.Close()
		if readErr != nil {
			log.Warn().Err(readErr).Str("url", url).Msg("Failed to read WA version body")
			continue
		}
		if status != http.StatusOK {
			log.Warn().Str("url", url).Int("status", status).Msg("Failed to fetch WA version")
			continue
		}
		parsed, err := parseWAVersion(body)
		if err != nil {
			log.Warn().Err(err).Str("url", url).Msg("Invalid WA version payload")
			continue
		}
		log.Debug().Str("url", url).Str("version", parsed.String()).Msg("WA version fetched")
		return &parsed
	}
	log.Warn().Msg("Failed to fetch WA version from all sources. Using local fallback.")
	return nil
}

// ResolveWAVersion resolve, cacheia e aplica (store.SetWAVersion) a versão a usar.
// Deve ser chamado no início de cada startClient (boot, /connect, reimport),
// antes de NewClient/Connect, equivalente ao bloco autoVersion do wbot.ts.
func ResolveWAVersion(ctx context.Context) store.WAVersionContainer {
	waVersionMu.Lock()
	defer waVersionMu.Unlock()

	if cached, found := waVersionCache.Get(waVersionCacheKey); found {
		if v, ok := cached.(store.WAVersionContainer); ok && !v.IsZero() {
			store.SetWAVersion(v)
			log.Debug().Str("version", v.String()).Msg("WA version from cache")
			return v
		}
		waVersionCache.Delete(waVersionCacheKey)
	}

	remote := fetchProjectWAVersion(ctx)
	var final store.WAVersionContainer
	if remote == nil {
		final = fallbackWAVersion
	} else {
		final = greaterWAVersion(*remote, fallbackWAVersion)
	}

	store.SetWAVersion(final)
	waVersionCache.Set(waVersionCacheKey, final, cache.DefaultExpiration)
	log.Info().Str("version", final.String()).Msg("using WA version")
	return final
}
