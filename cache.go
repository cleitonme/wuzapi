package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// CachedImage stores uploaded image data for reuse
type CachedImage struct {
	Upload    whatsmeow.UploadResponse
	MimeType  string
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

	// Statistics
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
		log.Debug().Msg("✅ Media cache initialized")
	}
}

// GetOrUploadImage checks cache first, uploads if not found
func (mc *MediaCache) GetOrUploadImage(ctx context.Context, client *whatsmeow.Client, filedata []byte, mimeType string) (whatsmeow.UploadResponse, error) {
	cacheKey := hashBytes(filedata)

	log.Debug().Str("cacheKey", cacheKey).Int("fileSize", len(filedata)).Msg("🔍 Checking image cache")

	if cached, ok := mc.images.Load(cacheKey); ok {
		cachedImg := cached.(*CachedImage)
		if time.Now().Before(cachedImg.ExpiresAt) {
			mc.imageHits++
			log.Debug().Str("cacheKey", cacheKey).Uint64("totalHits", mc.imageHits).Msg("✅ CACHE HIT - Using cached image upload")
			return cachedImg.Upload, nil
		}
		log.Debug().Str("cacheKey", cacheKey).Msg("⏰ Cache expired, removing")
		mc.images.Delete(cacheKey)
	}

	mc.imageMisses++
	log.Debug().Str("cacheKey", cacheKey).Uint64("totalMisses", mc.imageMisses).Msg("❌ CACHE MISS - Uploading new image")
	uploaded, err := client.Upload(ctx, filedata, whatsmeow.MediaImage)
	if err != nil {
		return whatsmeow.UploadResponse{}, err
	}

	expiresAt := time.Now().Add(24 * time.Hour)
	mc.images.Store(cacheKey, &CachedImage{
		Upload:    uploaded,
		MimeType:  mimeType,
		ExpiresAt: expiresAt,
	})

	log.Debug().Str("cacheKey", cacheKey).Time("expiresAt", expiresAt).Msg("💾 Cached image upload")

	return uploaded, nil
}

// GetOrUploadDocument similar to image but for documents
func (mc *MediaCache) GetOrUploadDocument(ctx context.Context, client *whatsmeow.Client, filedata []byte, mimeType string) (whatsmeow.UploadResponse, error) {
	cacheKey := hashBytes(filedata)

	log.Debug().Str("cacheKey", cacheKey).Int("fileSize", len(filedata)).Msg("🔍 Checking document cache")

	if cached, ok := mc.images.Load(cacheKey); ok {
		cachedDoc := cached.(*CachedImage)
		if time.Now().Before(cachedDoc.ExpiresAt) {
			mc.imageHits++
			log.Debug().Str("cacheKey", cacheKey).Uint64("totalHits", mc.imageHits).Msg("✅ CACHE HIT - Using cached document upload")
			return cachedDoc.Upload, nil
		}
		mc.images.Delete(cacheKey)
	}

	mc.imageMisses++
	log.Debug().Str("cacheKey", cacheKey).Uint64("totalMisses", mc.imageMisses).Msg("❌ CACHE MISS - Uploading new document")
	uploaded, err := client.Upload(ctx, filedata, whatsmeow.MediaDocument)
	if err != nil {
		return whatsmeow.UploadResponse{}, err
	}

	expiresAt := time.Now().Add(24 * time.Hour)
	mc.images.Store(cacheKey, &CachedImage{
		Upload:    uploaded,
		MimeType:  mimeType,
		ExpiresAt: expiresAt,
	})

	log.Debug().Str("cacheKey", cacheKey).Time("expiresAt", expiresAt).Msg("💾 Cached document upload")

	return uploaded, nil
}

// GetOrUploadVideo similar to image but for videos
func (mc *MediaCache) GetOrUploadVideo(ctx context.Context, client *whatsmeow.Client, filedata []byte, mimeType string) (whatsmeow.UploadResponse, error) {
	cacheKey := hashBytes(filedata)

	log.Debug().Str("cacheKey", cacheKey).Int("fileSize", len(filedata)).Msg("🔍 Checking video cache")

	if cached, ok := mc.images.Load(cacheKey); ok {
		cachedVid := cached.(*CachedImage)
		if time.Now().Before(cachedVid.ExpiresAt) {
			mc.imageHits++
			log.Debug().Str("cacheKey", cacheKey).Uint64("totalHits", mc.imageHits).Msg("✅ CACHE HIT - Using cached video upload")
			return cachedVid.Upload, nil
		}
		mc.images.Delete(cacheKey)
	}

	mc.imageMisses++
	log.Debug().Str("cacheKey", cacheKey).Uint64("totalMisses", mc.imageMisses).Msg("❌ CACHE MISS - Uploading new video")
	uploaded, err := client.Upload(ctx, filedata, whatsmeow.MediaVideo)
	if err != nil {
		return whatsmeow.UploadResponse{}, err
	}

	expiresAt := time.Now().Add(24 * time.Hour)
	mc.images.Store(cacheKey, &CachedImage{
		Upload:    uploaded,
		MimeType:  mimeType,
		ExpiresAt: expiresAt,
	})

	log.Debug().Str("cacheKey", cacheKey).Time("expiresAt", expiresAt).Msg("💾 Cached video upload")

	return uploaded, nil
}

// GetOrUploadAudio similar to image but for audio
func (mc *MediaCache) GetOrUploadAudio(ctx context.Context, client *whatsmeow.Client, filedata []byte, mimeType string) (whatsmeow.UploadResponse, error) {
	cacheKey := hashBytes(filedata)

	log.Debug().Str("cacheKey", cacheKey).Int("fileSize", len(filedata)).Msg("🔍 Checking audio cache")

	if cached, ok := mc.images.Load(cacheKey); ok {
		cachedAudio := cached.(*CachedImage)
		if time.Now().Before(cachedAudio.ExpiresAt) {
			mc.imageHits++
			log.Debug().Str("cacheKey", cacheKey).Uint64("totalHits", mc.imageHits).Msg("✅ CACHE HIT - Using cached audio upload")
			return cachedAudio.Upload, nil
		}
		mc.images.Delete(cacheKey)
	}

	mc.imageMisses++
	log.Debug().Str("cacheKey", cacheKey).Uint64("totalMisses", mc.imageMisses).Msg("❌ CACHE MISS - Uploading new audio")
	uploaded, err := client.Upload(ctx, filedata, whatsmeow.MediaAudio)
	if err != nil {
		return whatsmeow.UploadResponse{}, err
	}

	expiresAt := time.Now().Add(24 * time.Hour)
	mc.images.Store(cacheKey, &CachedImage{
		Upload:    uploaded,
		MimeType:  mimeType,
		ExpiresAt: expiresAt,
	})

	log.Debug().Str("cacheKey", cacheKey).Time("expiresAt", expiresAt).Msg("💾 Cached audio upload")

	return uploaded, nil
}

// GetGroupInfoCached returns cached group info or fetches if not available
func (mc *MediaCache) GetGroupInfoCached(ctx context.Context, client *whatsmeow.Client, jid types.JID) (*types.GroupInfo, error) {
	cacheKey := jid.String()

	log.Debug().Str("groupJID", cacheKey).Msg("🔍 Checking group info cache")

	if cached, ok := mc.groups.Load(cacheKey); ok {
		cachedGroup := cached.(*CachedGroupInfo)
		if time.Now().Before(cachedGroup.ExpiresAt) {
			mc.groupHits++
			participantCount := len(cachedGroup.Info.Participants)
			log.Debug().Str("groupJID", cacheKey).Int("participants", participantCount).Uint64("totalHits", mc.groupHits).Msg("✅ CACHE HIT - Using cached group info")
			return cachedGroup.Info, nil
		}
		log.Debug().Str("groupJID", cacheKey).Msg("⏰ Cache expired, removing")
		mc.groups.Delete(cacheKey)
	}

	mc.groupMisses++
	log.Debug().Str("groupJID", cacheKey).Uint64("totalMisses", mc.groupMisses).Msg("❌ CACHE MISS - Fetching group info from server")
	info, err := client.GetGroupInfo(ctx, jid)
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(5 * time.Minute)
	mc.groups.Store(cacheKey, &CachedGroupInfo{
		Info:      info,
		ExpiresAt: expiresAt,
	})

	participantCount := len(info.Participants)
	log.Debug().Str("groupJID", cacheKey).Int("participants", participantCount).Time("expiresAt", expiresAt).Msg("💾 Cached group info")

	return info, nil
}

// InvalidateGroupCache removes group from cache
func (mc *MediaCache) InvalidateGroupCache(jid types.JID) {
	mc.groups.Delete(jid.String())
	log.Debug().Str("groupJID", jid.String()).Msg("Invalidated group cache")
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
		log.Debug().Int("expiredImages", expiredImages).Int("expiredGroups", expiredGroups).Msg("🧹 Cleared expired cache entries")
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

	return map[string]interface{}{
		"cached_media":   imageCount,
		"cached_groups":  groupCount,
		"media_hits":     mc.imageHits,
		"media_misses":   mc.imageMisses,
		"group_hits":     mc.groupHits,
		"group_misses":   mc.groupMisses,
		"media_hit_rate": calculateHitRate(mc.imageHits, mc.imageMisses),
		"group_hit_rate": calculateHitRate(mc.groupHits, mc.groupMisses),
	}
}

func calculateHitRate(hits, misses uint64) float64 {
	total := hits + misses
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total) * 100
}

// Helper function to hash bytes for cache key
func hashBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}