package mirasim

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/thinking"
	"golang.org/x/net/websocket"
)

const spaceCatalogTimeout = 18 * time.Second

var spaceCatalogAgents = []string{"codex", "claude", "pi"}

type spaceGatewaySession struct {
	Session    string `json:"session"`
	NodeID     string `json:"nodeId"`
	GatewayURL string `json:"gatewayUrl"`
}

type spaceCatalogRow struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	ContextWindow int64  `json:"contextWindow"`
}

type spaceCatalogFrame struct {
	Type          string            `json:"type"`
	Agent         string            `json:"agent"`
	Models        []spaceCatalogRow `json:"models"`
	RouteByModel  map[string]string `json:"routeByModel"`
	OwnOnlyModels []json.RawMessage `json:"ownOnlyModels"`
}

func spaceCatalogError(message string) error {
	return spaceError(http.StatusBadGateway, "space_catalog_unavailable", message)
}

// A personal /v1/models catalog is filtered by that user's subscription. Space
// discovery instead opens the official, OAuth-authorized space connection and
// reads its workbench catalogs. These read-only frames do not start an agent,
// wake a sleeping space, spend credit, or require a model-turn capability.
func (c *Client) listSpaceModels(ctx context.Context) (Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, spaceCatalogTimeout)
	defer cancel()
	c.mu.Lock()
	node, err := c.spaceNodeLocked()
	if err == nil {
		err = c.loadLocked()
	}
	if err == nil {
		err = c.ensureAccessTokenLocked()
	}
	token, proxyURL, relayURL := c.accessToken, c.authProxyURL, c.storage.RelayURL
	c.mu.Unlock()
	if err != nil {
		return Catalog{}, err
	}
	session, err := c.createSpaceCatalogSession(ctx, node, token, proxyURL)
	if err != nil {
		return Catalog{}, err
	}
	ws, err := dialSpaceCatalog(ctx, session.GatewayURL, relayURL, proxyURL)
	if err != nil {
		return Catalog{}, spaceCatalogError("Cannot connect to the selected space's model catalog")
	}
	defer ws.Close()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	ws.MaxPayloadBytes = 4 << 20
	if err := websocket.JSON.Send(ws, map[string]any{"type": "gatewayAuth", "token": session.Session, "nodeId": node, "wake": 0}); err != nil {
		return Catalog{}, spaceCatalogError("Cannot authorize the space model catalog connection")
	}
	remaining := make(map[string]bool, len(spaceCatalogAgents))
	for _, agent := range spaceCatalogAgents {
		remaining[agent] = true
	}
	requested := false
	byID := make(map[string]RemoteModel)
	roster := ModelRoster{Version: "space:" + node, Models: make(map[string]ModelSpec)}
	for len(remaining) > 0 {
		var frame spaceCatalogFrame
		if err := websocket.JSON.Receive(ws, &frame); err != nil {
			return Catalog{}, spaceCatalogError("The space model catalog connection ended before all agent catalogs were received")
		}
		switch frame.Type {
		case "gatewayHostCaps":
			if requested {
				continue
			}
			requested = true
			for _, agent := range spaceCatalogAgents {
				if err := websocket.JSON.Send(ws, map[string]string{"type": "getCatalog", "agent": agent}); err != nil {
					return Catalog{}, spaceCatalogError("Cannot request the space's agent model catalogs")
				}
			}
		case "error":
			return Catalog{}, spaceCatalogError("The space gateway refused model discovery; check space access and availability")
		case "catalog":
			if !requested || !remaining[frame.Agent] {
				continue
			}
			if frame.Models == nil {
				return Catalog{}, spaceCatalogError("The space gateway returned an invalid model catalog")
			}
			mergeSpaceCatalog(frame, byID, &roster)
			delete(remaining, frame.Agent)
		}
	}
	if len(byID) == 0 {
		return Catalog{}, spaceCatalogError("The selected space publishes no supported platform models")
	}
	models := make([]RemoteModel, 0, len(byID))
	for _, model := range byID {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	c.catalogMu.Lock()
	c.catalogModels = append([]RemoteModel(nil), models...)
	c.catalogMu.Unlock()
	c.rosterMu.Lock()
	c.roster = roster
	c.rosterMu.Unlock()
	return Catalog{Models: models}, nil
}

func mergeSpaceCatalog(frame spaceCatalogFrame, models map[string]RemoteModel, roster *ModelRoster) {
	own := make(map[string]bool)
	for _, raw := range frame.OwnOnlyModels {
		if model, ok := parseCatalogEntry(raw); ok {
			own[strings.ToLower(thinking.ParseModel(model.ID).ModelName)] = true
		}
	}
	for _, row := range frame.Models {
		id := strings.ToLower(thinking.ParseModel(row.ID).ModelName)
		route := frame.RouteByModel[row.ID]
		if own[id] || route == "direct" || route == "own" || !spacePlatformModel(id) {
			continue
		}
		model, exists := models[id]
		if !exists {
			model = RemoteModel{ID: id, Object: "model", OwnedBy: "mirasim-space"}
		}
		if row.ContextWindow > model.MaxInputTokens {
			model.MaxInputTokens = row.ContextWindow
		}
		models[id] = model
		spec := roster.Models[id]
		spec.ID = id
		if spec.Label == "" {
			spec.Label = strings.TrimSpace(row.Label)
		}
		spec.ContextWindow = model.MaxInputTokens
		roster.Models[id] = spec
	}
}

func spacePlatformModel(id string) bool {
	if PaidVariantModel(id) {
		return false
	}
	for _, prefix := range []string{"gpt-", "claude-", "deepseek-", "glm-", "kimi-", "gemini-"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

func (c *Client) createSpaceCatalogSession(ctx context.Context, node, token, proxyURL string) (spaceGatewaySession, error) {
	var session spaceGatewaySession
	endpoint, _, err := c.endpoint("/v2/gateway/session", nil)
	if err != nil {
		return session, err
	}
	body, _ := json.Marshal(map[string]string{"nodeId": node})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return session, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	private, closeClient, err := newPrivateHTTPClient(proxyURL, spaceCatalogTimeout)
	if err != nil {
		return session, err
	}
	defer closeClient()
	resp, err := private.Do(req)
	if err != nil {
		return session, spaceCatalogError("Cannot create a space model catalog session")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			c.markAccessRefreshRequired()
		}
		return session, spaceError(resp.StatusCode, "space_catalog_session", "Mirasim refused the selected space's catalog session")
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(&session) != nil || session.NodeID != node || session.Session == "" || session.GatewayURL == "" {
		return session, spaceCatalogError("Mirasim returned an invalid or mismatched space catalog session")
	}
	return session, nil
}

func dialSpaceCatalog(ctx context.Context, gatewayURL, relayURL, proxyURL string) (*websocket.Conn, error) {
	gateway, err := url.Parse(gatewayURL)
	relay, relayErr := url.Parse(relayURL)
	if err != nil || relayErr != nil || gateway.User != nil || !strings.EqualFold(gateway.Host, relay.Host) ||
		!((relay.Scheme == "https" && gateway.Scheme == "wss") || (relay.Scheme == "http" && gateway.Scheme == "ws")) {
		return nil, fmt.Errorf("space gateway must use the configured relay origin")
	}
	if proxyURL == "" {
		proxyRequest := &http.Request{URL: &url.URL{Scheme: relay.Scheme, Host: gateway.Host}}
		if environmentProxy, err := http.ProxyFromEnvironment(proxyRequest); err == nil && environmentProxy != nil {
			proxyURL = environmentProxy.String()
		}
	}
	dialer, _, err := proxyutil.BuildDialer(proxyURL)
	if err != nil {
		return nil, err
	}
	if dialer == nil {
		dialer = &net.Dialer{Timeout: spaceCatalogTimeout}
	}
	contextDialer, ok := dialer.(interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	})
	if !ok {
		return nil, fmt.Errorf("space gateway proxy does not support context dialing")
	}
	port := gateway.Port()
	if port == "" {
		port = "443"
		if gateway.Scheme == "ws" {
			port = "80"
		}
	}
	conn, err := contextDialer.DialContext(ctx, "tcp", net.JoinHostPort(gateway.Hostname(), port))
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = conn.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stop()
	if gateway.Scheme == "wss" {
		secure := tls.Client(conn, &tls.Config{ServerName: gateway.Hostname(), MinVersion: tls.VersionTLS12})
		if err := secure.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = secure
	}
	config, err := websocket.NewConfig(gatewayURL, relay.Scheme+"://"+relay.Host)
	if err != nil {
		return nil, err
	}
	ws, err := websocket.NewClient(config, conn)
	if err == nil {
		success = true
	}
	return ws, err
}
