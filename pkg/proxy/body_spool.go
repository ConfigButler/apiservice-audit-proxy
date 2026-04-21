package proxy

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

const copyBufferSize = 32 * 1024

type spooledBody struct {
	path      string
	captured  []byte
	size      int64
	truncated bool
}

func spoolBody(body io.ReadCloser, tempDir string, maxCaptureBytes int64) (*spooledBody, error) {
	if body == nil {
		body = http.NoBody
	}
	defer func() {
		_ = body.Close()
	}()

	file, err := os.CreateTemp(tempDir, "audit-pass-through-body-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}

	result := &spooledBody{
		path:     file.Name(),
		captured: make([]byte, 0, bufferCapacity(maxCaptureBytes)),
	}

	buffer := make([]byte, copyBufferSize)
	for {
		n, readErr := body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			written, writeErr := file.Write(chunk)
			if writeErr != nil {
				_ = file.Close()
				_ = os.Remove(file.Name())
				return nil, fmt.Errorf("write temp file: %w", writeErr)
			}
			result.size += int64(written)

			if int64(len(result.captured)) < maxCaptureBytes {
				remaining := int(maxCaptureBytes - int64(len(result.captured)))
				if remaining > len(chunk) {
					remaining = len(chunk)
				}
				result.captured = append(result.captured, chunk[:remaining]...)
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			return nil, fmt.Errorf("read body: %w", readErr)
		}
	}

	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return nil, fmt.Errorf("close temp file: %w", err)
	}

	result.truncated = result.size > maxCaptureBytes
	return result, nil
}

func bufferCapacity(maxCaptureBytes int64) int {
	if maxCaptureBytes <= 0 {
		return 0
	}

	return int(maxCaptureBytes)
}

func (s *spooledBody) Open() (*os.File, error) {
	file, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("open temp file: %w", err)
	}

	return file, nil
}

func (s *spooledBody) Cleanup() error {
	if s == nil || s.path == "" {
		return nil
	}

	return os.Remove(s.path)
}
