package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"syscall"
)

// memaModels asks whisker to free its ASR models (POST /v1/unload), so a chat
// request can have the GPU. whisker loads them again by itself on the next
// transcription. The TTS server speaks the same /v1/unload contract, so it is
// driven by this type too, with name "tts".
type memaModels struct {
	name   string // for the logs; "" means "whisker"
	base   string // "http://127.0.0.1:8788"
	token  string // whisker's WHISKER_AUTH_TOKEN
	client *http.Client
}

func (m *memaModels) label() string {
	if m.name == "" {
		return "whisker"
	}
	return m.name
}

// evict frees the server's models. A server that is not listening holds no GPU
// memory, so that also returns nil.
func (m *memaModels) evict(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+"/v1/unload", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := m.client.Do(req)
	if errors.Is(err, syscall.ECONNREFUSED) {
		log.Printf("%s not listening (%v); nothing to unload", m.label(), err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("POST /v1/unload: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /v1/unload: status %d", resp.StatusCode)
	}
	log.Printf("%s unloaded: %s", m.label(), body)
	return nil
}
