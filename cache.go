package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const (
	mediaTTL = 5 * time.Minute
	groupTTL = 24 * time.Hour
)

// CachedImage stores uploaded image data for reuse
type CachedImage struct {
	Upload    whatsmeow.UploadResponse
	MimeType  string
	HashFull  string
	Thumbnail []byte
	ExpiresAt time.Time
}

// CachedGroupInfo stores group information
type CachedGroupInfo struct {
	Info      *types.GroupInfo
	ExpiresAt time.Time
}

// MediaCache manages cached media uploads and group info
type MediaCache struct {
	images sync.Map // map[string]*CachedImage
	groups sync.Map // map[string]*CachedGroupInfo

	// Statistics (atomic for thread safety)
	imageHits   uint64
	imageMisses uint64
	groupHits   uint64
	groupMisses uint64
}

var mediaCache *MediaCache

// InitMediaCache initializes the global media cache
func InitMediaCache() {
	if mediaCache == nil {
		mediaCache = &MediaCache{}
		log.Info().Msg("✅ Media cache initialized")

		// Start background cleanup goroutine
		go mediaCache.startCleanupWorker()
	}
}

// startCleanupWorker runs periodic cleanup of expired entries
func (mc *MediaCache) startCleanupWorker() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		mc.ClearExpired()
	}
}

// uploadMedia is a generic upload helper to avoid code duplication
func (mc *MediaCache) uploadMedia(
	ctx context.Context,
	client *whatsmeow.Client,
	filedata []byte,
	mimeType string,
	mediaType whatsmeow.MediaType,
	label string,
) (whatsmeow.UploadResponse, error) {

	quickKey := quickHash(filedata)

	log.Info().
		Str("quickKey", quickKey).
		Int("fileSize", len(filedata)).
		Str("type", label).
		Msg("🔍 Checking media cache")

	// 🔎 tenta cache pelo quick hash
	if cached, ok := mc.images.Load(quickKey); ok {
		cachedMedia := cached.(*CachedImage)

		if time.Now().Before(cachedMedia.ExpiresAt) {

			// 🔒 valida com hash completo (evita colisão falsa)
			fullHash := hashBytes(filedata)

			if cachedMedia.HashFull == fullHash {
				hits := atomic.AddUint64(&mc.imageHits, 1)

				log.Info().
					Str("quickKey", quickKey).
					Str("type", label).
					Uint64("totalHits", hits).
					Msg("✅ CACHE HIT")

				return cachedMedia.Upload, nil
			}

			log.Warn().
				Str("quickKey", quickKey).
				Msg("⚠️ Hash collision detected, ignoring cache")
		}

		mc.images.Delete(quickKey)
	}

	// ❌ cache miss
	misses := atomic.AddUint64(&mc.imageMisses, 1)

	log.Info().
		Str("quickKey", quickKey).
		Str("type", label).
		Uint64("totalMisses", misses).
		Msg("❌ CACHE MISS")

	// 🔥 só aqui calcula SHA256 completo
	fullHash := hashBytes(filedata)

	uploaded, err := client.Upload(ctx, filedata, mediaType)
	if err != nil {
		return whatsmeow.UploadResponse{}, err
	}

	expiresAt := time.Now().Add(mediaTTL)

	mc.images.Store(quickKey, &CachedImage{
		Upload:    uploaded,
		MimeType:  mimeType,
		HashFull:  fullHash,
		ExpiresAt: expiresAt,
	})

	log.Info().
		Str("quickKey", quickKey).
		Time("expiresAt", expiresAt).
		Msg("💾 Cached media upload")

	return uploaded, nil
}

// GetOrUploadImage checks cache first, uploads if not found
func (mc *MediaCache) GetOrUploadImage(ctx context.Context, client *whatsmeow.Client, filedata []byte, mimeType string) (whatsmeow.UploadResponse, error) {
	return mc.uploadMedia(ctx, client, filedata, mimeType, whatsmeow.MediaImage, "image")
}

// GetOrUploadDocument checks cache first, uploads if not found
func (mc *MediaCache) GetOrUploadDocument(ctx context.Context, client *whatsmeow.Client, filedata []byte, mimeType string) (whatsmeow.UploadResponse, error) {
	return mc.uploadMedia(ctx, client, filedata, mimeType, whatsmeow.MediaDocument, "document")
}

// GetOrUploadVideo checks cache first, uploads if not found
func (mc *MediaCache) GetOrUploadVideo(ctx context.Context, client *whatsmeow.Client, filedata []byte, mimeType string) (whatsmeow.UploadResponse, error) {
	return mc.uploadMedia(ctx, client, filedata, mimeType, whatsmeow.MediaVideo, "video")
}

// GetOrUploadAudio checks cache first, uploads if not found
func (mc *MediaCache) GetOrUploadAudio(ctx context.Context, client *whatsmeow.Client, filedata []byte, mimeType string) (whatsmeow.UploadResponse, error) {
	return mc.uploadMedia(ctx, client, filedata, mimeType, whatsmeow.MediaAudio, "audio")
}

// GetGroupInfoCached returns cached group info or fetches if not available
func (mc *MediaCache) GetGroupInfoCached(ctx context.Context, client *whatsmeow.Client, jid types.JID) (*types.GroupInfo, error) {
	cacheKey := jid.String()

	log.Info().Str("groupJID", cacheKey).Msg("🔍 Checking group info cache")

	if cached, ok := mc.groups.Load(cacheKey); ok {
		cachedGroup := cached.(*CachedGroupInfo)
		if time.Now().Before(cachedGroup.ExpiresAt) {
			hits := atomic.AddUint64(&mc.groupHits, 1)
			participantCount := len(cachedGroup.Info.Participants)
			log.Info().Str("groupJID", cacheKey).Int("participants", participantCount).Uint64("totalHits", hits).Msg("✅ CACHE HIT - Using cached group info")
			return cachedGroup.Info, nil
		}
		log.Info().Str("groupJID", cacheKey).Msg("⏰ Cache expired, removing")
		mc.groups.Delete(cacheKey)
	}

	misses := atomic.AddUint64(&mc.groupMisses, 1)
	log.Info().Str("groupJID", cacheKey).Uint64("totalMisses", misses).Msg("❌ CACHE MISS - Fetching group info from server")

	info, err := client.GetGroupInfo(ctx, jid)
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(groupTTL)
	mc.groups.Store(cacheKey, &CachedGroupInfo{
		Info:      info,
		ExpiresAt: expiresAt,
	})

	participantCount := len(info.Participants)
	log.Info().Str("groupJID", cacheKey).Int("participants", participantCount).Time("expiresAt", expiresAt).Msg("💾 Cached group info")

	return info, nil
}

// InvalidateGroupCache removes group from cache
func (mc *MediaCache) InvalidateGroupCache(jid types.JID) {
	mc.groups.Delete(jid.String())
	log.Info().Str("groupJID", jid.String()).Msg("🗑️ Invalidated group cache")
}

// ClearExpired removes expired entries from cache
func (mc *MediaCache) ClearExpired() {
	now := time.Now()
	expiredImages := 0
	expiredGroups := 0

	mc.images.Range(func(key, value interface{}) bool {
		cached := value.(*CachedImage)
		if now.After(cached.ExpiresAt) {
			mc.images.Delete(key)
			expiredImages++
		}
		return true
	})

	mc.groups.Range(func(key, value interface{}) bool {
		cached := value.(*CachedGroupInfo)
		if now.After(cached.ExpiresAt) {
			mc.groups.Delete(key)
			expiredGroups++
		}
		return true
	})

	if expiredImages > 0 || expiredGroups > 0 {
		log.Info().Int("expiredImages", expiredImages).Int("expiredGroups", expiredGroups).Msg("🧹 Cleared expired cache entries")
	}
}

// GetStats returns cache statistics
func (mc *MediaCache) GetStats() map[string]interface{} {
	imageCount := 0
	groupCount := 0

	mc.images.Range(func(key, value interface{}) bool {
		imageCount++
		return true
	})

	mc.groups.Range(func(key, value interface{}) bool {
		groupCount++
		return true
	})

	imageHits := atomic.LoadUint64(&mc.imageHits)
	imageMisses := atomic.LoadUint64(&mc.imageMisses)
	groupHits := atomic.LoadUint64(&mc.groupHits)
	groupMisses := atomic.LoadUint64(&mc.groupMisses)

	return map[string]interface{}{
		"cached_media":   imageCount,
		"cached_groups":  groupCount,
		"media_hits":     imageHits,
		"media_misses":   imageMisses,
		"group_hits":     groupHits,
		"group_misses":   groupMisses,
		"media_hit_rate": calculateHitRate(imageHits, imageMisses),
		"group_hit_rate": calculateHitRate(groupHits, groupMisses),
	}
}

func calculateHitRate(hits, misses uint64) float64 {
	total := hits + misses
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total) * 100
}

// hashBytes hashes bytes for cache key
func hashBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func quickHash(data []byte) string {
	h := sha256.New()

	size := len(data)

	// primeiros 256 bytes
	if size > 256 {
		h.Write(data[:256])
	} else {
		h.Write(data)
	}

	// tamanho do arquivo (importante!)
	_ = binary.Write(h, binary.LittleEndian, int64(size))

	// últimos 256 bytes (se existir)
	if size > 256 {
		h.Write(data[size-256:])
	}

	return hex.EncodeToString(h.Sum(nil))
}
