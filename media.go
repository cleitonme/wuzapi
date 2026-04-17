package main

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/vincent-petithory/dataurl"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

const (
	downloadTimeoutImage    = 2 * time.Minute
	downloadTimeoutAudio    = 5 * time.Minute
	downloadTimeoutDocument = 10 * time.Minute
	downloadTimeoutVideo    = 10 * time.Minute
	downloadTimeoutSticker  = 1 * time.Minute
)

type mediaS3Config struct {
	Enabled       string
	MediaDelivery string
}

// DownloadResult é retornado por downloadMedia para handlers HTTP
type DownloadResult struct {
	Data     []byte
	MimeType string
	DataURL  string // dataurl.New(data, mime).String()
}

// downloadMedia faz download com timeout e retorna os bytes + dataurl pronto.
// Usado pelos handlers DownloadImage, DownloadAudio, DownloadVideo, DownloadDocument, DownloadSticker.
func downloadMedia(
	ctx context.Context,
	client *whatsmeow.Client,
	msg whatsmeow.DownloadableMessage,
	mimeType string,
	timeout time.Duration,
) (*DownloadResult, error) {
	dlCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	data, err := client.Download(dlCtx, msg)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}

	du := dataurl.New(data, mimeType)
	return &DownloadResult{
		Data:     data,
		MimeType: mimeType,
		DataURL:  du.String(),
	}, nil
}

// buildImageMsg, buildAudioMsg, buildVideoMsg, buildDocumentMsg, buildStickerMsg
// constroem o waE2E.Message a partir dos campos recebidos pelo handler,
// permitindo reusar downloadMedia sem duplicar a montagem do proto.

type mediaDownloadParams struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	MimeType      string
	FileEncSHA256 []byte
	FileSHA256    []byte
	FileLength    uint64
}

func buildAndDownloadImage(ctx context.Context, client *whatsmeow.Client, p mediaDownloadParams) (*DownloadResult, error) {
	msg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		URL: &p.URL, DirectPath: &p.DirectPath, MediaKey: p.MediaKey,
		Mimetype: &p.MimeType, FileEncSHA256: p.FileEncSHA256,
		FileSHA256: p.FileSHA256, FileLength: &p.FileLength,
	}}
	return downloadMedia(ctx, client, msg.GetImageMessage(), p.MimeType, downloadTimeoutImage)
}

func buildAndDownloadAudio(ctx context.Context, client *whatsmeow.Client, p mediaDownloadParams) (*DownloadResult, error) {
	msg := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		URL: &p.URL, DirectPath: &p.DirectPath, MediaKey: p.MediaKey,
		Mimetype: &p.MimeType, FileEncSHA256: p.FileEncSHA256,
		FileSHA256: p.FileSHA256, FileLength: &p.FileLength,
	}}
	return downloadMedia(ctx, client, msg.GetAudioMessage(), p.MimeType, downloadTimeoutAudio)
}

func buildAndDownloadVideo(ctx context.Context, client *whatsmeow.Client, p mediaDownloadParams) (*DownloadResult, error) {
	msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
		URL: &p.URL, DirectPath: &p.DirectPath, MediaKey: p.MediaKey,
		Mimetype: &p.MimeType, FileEncSHA256: p.FileEncSHA256,
		FileSHA256: p.FileSHA256, FileLength: &p.FileLength,
	}}
	return downloadMedia(ctx, client, msg.GetVideoMessage(), p.MimeType, downloadTimeoutVideo)
}

func buildAndDownloadDocument(ctx context.Context, client *whatsmeow.Client, p mediaDownloadParams) (*DownloadResult, error) {
	msg := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		URL: &p.URL, DirectPath: &p.DirectPath, MediaKey: p.MediaKey,
		Mimetype: &p.MimeType, FileEncSHA256: p.FileEncSHA256,
		FileSHA256: p.FileSHA256, FileLength: &p.FileLength,
	}}
	return downloadMedia(ctx, client, msg.GetDocumentMessage(), p.MimeType, downloadTimeoutDocument)
}

func buildAndDownloadSticker(ctx context.Context, client *whatsmeow.Client, p mediaDownloadParams) (*DownloadResult, error) {
	msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
		URL: &p.URL, DirectPath: &p.DirectPath, MediaKey: p.MediaKey,
		Mimetype: &p.MimeType, FileEncSHA256: p.FileEncSHA256,
		FileSHA256: p.FileSHA256, FileLength: &p.FileLength,
	}}
	return downloadMedia(ctx, client, msg.GetStickerMessage(), p.MimeType, downloadTimeoutSticker)
}

func (mycli *MyClient) processMedia(
	msg whatsmeow.DownloadableMessage,
	mimeType string,
	fallbackExt string,
	timeout time.Duration,
	isIncoming bool,
	chatJID string,
	messageID string,
	s3cfg mediaS3Config,
	postmap map[string]interface{},
	extraKeys map[string]interface{},
) {
	tmpDir := filepath.Join("/tmp", "user_"+mycli.userID)
	if err := os.MkdirAll(tmpDir, 0751); err != nil {
		log.Error().Err(err).Msg("Could not create temporary directory")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	data, err := mycli.WAClient.Download(ctx, msg)
	if err != nil {
		log.Error().Err(err).Msg("Failed to download media")
		return
	}

	ext := fallbackExt
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		ext = exts[0]
	}
	tmpPath := filepath.Join(tmpDir, messageID+ext)

	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		log.Error().Err(err).Msg("Failed to save media to temporary file")
		return
	}
	defer func() {
		if err := os.Remove(tmpPath); err != nil {
			log.Error().Err(err).Msg("Failed to delete temporary file")
		} else {
			log.Info().Str("path", tmpPath).Msg("Temporary file deleted")
		}
	}()

	if s3cfg.Enabled == "true" && (s3cfg.MediaDelivery == "s3" || s3cfg.MediaDelivery == "both") {
		s3Data, err := GetS3Manager().ProcessMediaForS3(
			context.Background(),
			mycli.userID,
			chatJID,
			messageID,
			data,
			mimeType,
			filepath.Base(tmpPath),
			isIncoming,
		)
		if err != nil {
			log.Error().Err(err).Msg("Failed to upload media to S3")
		} else {
			postmap["s3"] = s3Data
		}
	}

	if s3cfg.MediaDelivery == "base64" || s3cfg.MediaDelivery == "both" {
		b64, mime_, err := fileToBase64(tmpPath)
		if err != nil {
			log.Error().Err(err).Msg("Failed to convert media to base64")
			return
		}
		postmap["base64"] = b64
		postmap["mimeType"] = mime_
		postmap["fileName"] = filepath.Base(tmpPath)
	}

	for k, v := range extraKeys {
		postmap[k] = v
	}

	log.Info().Str("path", tmpPath).Msg("Media processed")
}
