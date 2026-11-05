package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RESTBase derives the REST API root from the websocket URL:
// ws://host:8123/api/websocket -> http://host:8123/api, and for add-ons
// ws://supervisor/core/websocket -> http://supervisor/core/api.
func RESTBase(wsURL string) (string, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("not a websocket URL: %s", wsURL)
	}
	p := strings.TrimSuffix(u.Path, "/")
	switch {
	case strings.HasSuffix(p, "/api/websocket"):
		p = strings.TrimSuffix(p, "/websocket")
	case strings.HasSuffix(p, "/websocket"): // Supervisor proxy
		p = strings.TrimSuffix(p, "/websocket") + "/api"
	default:
		return "", fmt.Errorf("unexpected websocket path %q", u.Path)
	}
	u.Path = p
	return u.String(), nil
}

// States sets entity states through the REST API (POST /api/states/<id>).
// Such entities aren't owned by an integration: they disappear when Home
// Assistant restarts, until set again.
type States struct {
	Base  string // from RESTBase
	Token string
	HTTP  *http.Client
}

// Set creates or updates an entity's state and attributes.
func (s States) Set(ctx context.Context, entityID, state string, attrs map[string]any) error {
	body, err := json.Marshal(map[string]any{"state": state, "attributes": attrs})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Base+"/states/"+entityID, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("set %s: %w", entityID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("set %s: %w (HTTP %d)", entityID, ErrAuth, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("set %s: HTTP %d: %s", entityID, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
