package main

import (
	"context"
	"fmt"
	"time"

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
