package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

// llamaModels drives the llama.cpp router's model API, so the gate can push
// llama's model out of VRAM before a transcription. The router loads it again
// by itself on the next chat request (autoload is on).
type llamaModels struct {
	base    string // "http://127.0.0.1:8081"
	key     string // the router's --api-key; empty sends no auth
	client  *http.Client
	poll    time.Duration // how often to re-check /models after unloading
	timeout time.Duration // give up on the whole eviction after this long
}

type routerModel struct {
	ID     string `json:"id"`
	Status struct {
		Value string `json:"value"` // "loaded", "loading" or "unloaded"
	} `json:"status"`
}

type modelList struct {
	Data []routerModel `json:"data"`
}

// errUnreachable means nothing answered at all: llama is down, so it holds no
// GPU memory.
var errUnreachable = errors.New("llama-server unreachable")

// evict unloads every resident model and waits until the router reports none,
// so the GPU memory is free when it returns nil. A router that does not answer
// at all holds nothing, so that also returns nil.
func (l *llamaModels) evict(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	ids, err := l.resident(ctx)
	if errors.Is(err, errUnreachable) {
		log.Printf("%v; forwarding the transcription anyway", err)
		return nil
	}
	if err != nil || len(ids) == 0 {
		return err
	}
	log.Printf("unloading llama models %v for whisker", ids)
	for _, id := range ids {
		if err := l.unload(ctx, id); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("llama models %v still loaded after %s", ids, l.timeout)
		case <-time.After(l.poll):
		}
		if ids, err = l.resident(ctx); err != nil || len(ids) == 0 {
			return err
		}
	}
}

// resident lists the models that are loaded or loading.
func (l *llamaModels) resident(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.base+"/models", nil)
	if err != nil {
		return nil, err
	}
	l.auth(req)
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /models: status %d", resp.StatusCode)
	}
	var list modelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("GET /models: %v", err)
	}
	var ids []string
	for _, m := range list.Data {
		if m.Status.Value == "loaded" || m.Status.Value == "loading" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func (l *llamaModels) unload(ctx context.Context, id string) error {
	body, _ := json.Marshal(map[string]string{"model": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base+"/models/unload", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	l.auth(req)
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("unload %s: %v", id, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unload %s: status %d", id, resp.StatusCode)
	}
	return nil
}

func (l *llamaModels) auth(req *http.Request) {
	if l.key != "" {
		req.Header.Set("Authorization", "Bearer "+l.key)
	}
}
