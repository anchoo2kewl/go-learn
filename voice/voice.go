// Package voice talks to the local speech sidecar (sidecar/sidecar.py): text to speech with word
// timings for read-along, and speech to text for spoken questions. The sidecar keeps the models
// loaded and runs on Apple Silicon (MLX); it listens on localhost only.
package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Word is one script word with its spoken time span, in seconds.
type Word struct {
	P int     `json:"p"` // paragraph index
	W string  `json:"w"`
	S float64 `json:"s"`
	E float64 `json:"e"`
}

// Rendered is a voiced segment.
type Rendered struct {
	DurationS  float64 `json:"duration_s"`
	Words      []Word  `json:"words"`
	AlignScore float64 `json:"align_score"`
}

// Voice is one entry in the stock catalogue.
type Voice struct {
	ID     string `json:"id"`
	Engine string `json:"engine"`
	Label  string `json:"label"`
	Accent string `json:"accent"`
	Gender string `json:"gender"`
}

// Speaker turns text into audio; Transcriber turns audio into text.
type Speaker interface {
	Render(ctx context.Context, voiceID string, display, speech []string, outWav string) (*Rendered, error)
	Voices(ctx context.Context) ([]Voice, error)
	Preview(ctx context.Context, voiceID, outWav string) error
}

type Transcriber interface {
	Transcribe(ctx context.Context, wavPath string) (string, error)
}

// Client is the HTTP client for the sidecar.
type Client struct {
	URL  string // e.g. http://127.0.0.1:8791
	HTTP *http.Client
}

func (c *Client) post(ctx context.Context, path string, in, out any, timeout time.Duration) error {
	b, _ := json.Marshal(in)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("voice sidecar: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("voice sidecar %s: %s: %s", path, resp.Status, bytes.TrimSpace(body))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *Client) Render(ctx context.Context, voiceID string, display, speech []string, outWav string) (*Rendered, error) {
	var r Rendered
	err := c.post(ctx, "/render", map[string]any{"voice_id": voiceID, "display": display, "speech": speech, "out": outWav}, &r, 20*time.Minute)
	return &r, err
}

func (c *Client) Transcribe(ctx context.Context, wavPath string) (string, error) {
	var r struct {
		Text string `json:"text"`
	}
	err := c.post(ctx, "/transcribe", map[string]any{"path": wavPath}, &r, 3*time.Minute)
	return r.Text, err
}

func (c *Client) Preview(ctx context.Context, voiceID, outWav string) error {
	return c.post(ctx, "/preview", map[string]any{"voice_id": voiceID, "out": outWav}, nil, 5*time.Minute)
}

func (c *Client) Voices(ctx context.Context) ([]Voice, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/voices", nil)
	if err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voice sidecar: %w", err)
	}
	defer resp.Body.Close()
	var v []Voice
	return v, json.NewDecoder(resp.Body).Decode(&v)
}

// Healthy reports whether the sidecar answers.
func (c *Client) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
