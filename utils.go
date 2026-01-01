package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"math/rand"

	"github.com/google/uuid"
)

func downloadMedia(url string) ([]byte, string, error) {
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, "", fmt.Errorf("failed to download media: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("failed to download media, status code: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read media body: %v", err)
	}

	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}

	return data, mimeType, nil
}

func convertToOpus(inputData []byte) ([]byte, error) {
	tempID := uuid.New().String()
	inputFile := filepath.Join(os.TempDir(), "wuzapi_in_"+tempID)
	outputFile := filepath.Join(os.TempDir(), "wuzapi_out_"+tempID+".ogg")

	defer os.Remove(inputFile)
	defer os.Remove(outputFile)

	if err := os.WriteFile(inputFile, inputData, 0644); err != nil {
		return nil, fmt.Errorf("failed to write temp file: %v", err)
	}

	cmd := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-i", inputFile,
		"-c:a", "libopus",
		"-b:a", "64k",
		"-vbr", "on",
		"-compression_level", "10",
		"-ac", "1",
		"-ar", "48000",
		"-vn",
		outputFile,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg error: %v | Log: %s", err, stderr.String())
	}

	convertedData, err := os.ReadFile(outputFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read converted file: %v", err)
	}

	return convertedData, nil
}

func generateFakeWaveform() []byte {
	waveform := make([]byte, 64)

	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < 64; i++ {
		val := r.Intn(100)
		if val < 20 {
			val = 10
		}
		waveform[i] = byte(val)
	}
	return waveform
}
