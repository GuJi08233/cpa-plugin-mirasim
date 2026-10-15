package mirasim

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	spaceGatewayPath = "/v2/gateway/model"
	spaceRenewPath   = "/v2/gateway/turn-capability/renew"
	spacePayerHeader = "X-Mirasim-Payer"
	spaceRefreshLead = 90 * time.Second
)

var spaceNodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// SpaceCredit amounts use the platform's integer credit unit: 100 credits
// display as one US dollar. Never confuse these with token counts.
type SpaceCredit struct {
	NodeID  string `json:"nodeId"`
	Mode    string `json:"mode"`
	Balance int64  `json:"balance"`
	Funded  int64  `json:"funded"`
	Used    int64  `json:"used"`
	CapMode string `json:"capMode"`
	Me      struct {
		Cap       *int64 `json:"cap"`
		Used      int64  `json:"used"`
		Remaining *int64 `json:"remaining"`
		Prefer    string `json:"prefer"`
		Payer     string `json:"payer"`
		Reason    string `json:"reason,omitempty"`
	} `json:"me"`
}

func (s SpaceCredit) Clone() *SpaceCredit {
	clone := s
	if s.Me.Cap != nil {
		v := *s.Me.Cap
		clone.Me.Cap = &v
	}
	if s.Me.Remaining != nil {
		v := *s.Me.Remaining
		clone.Me.Remaining = &v
	}
	return &clone
}

func (c *Client) SpaceEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.options.SpaceNodeID != "" || c.options.SpaceCapabilityFile != ""
}

func (c *Client) spaceNodeLocked() (string, error) {
	node := strings.TrimSpace(c.options.SpaceNodeID)
	if node == "" {
		node = strings.TrimSpace(c.storage.SpaceNodeID)
	}
	if !spaceNodePattern.MatchString(node) {
		return "", spaceError(http.StatusBadRequest, "space_configuration", "space-node-id must be a valid Mirasim space node ID")
	}
	return node, nil
}

func spaceError(status int, code, message string) error {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": code, "message": message}})
	return NewStatusError(status, body, nil)
}

// FetchSpaceCredit uses the user's OAuth login to read the selected space.
// This API accepts neither a device ticket nor a model-turn capability.
func (c *Client) FetchSpaceCredit(ctx context.Context, client pluginapi.HostHTTPClient) (SpaceCredit, error) {
	if client == nil {
		return SpaceCredit{}, fmt.Errorf("host HTTP client is required")
	}
	c.mu.Lock()
	node, err := c.spaceNodeLocked()
	if err == nil {
		err = c.loadLocked()
	}
	if err == nil {
		err = c.ensureAccessTokenLocked()
	}
	token := c.accessToken
	c.mu.Unlock()
	if err != nil {
		return SpaceCredit{}, err
	}
	endpoint, _, err := c.endpoint("/v2/machines/"+node+"/credit", nil)
	if err != nil {
		return SpaceCredit{}, err
	}
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{
		Method: http.MethodGet, URL: endpoint,
		Headers: http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/json"}},
	})
	if err != nil {
		return SpaceCredit{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized {
			c.markAccessRefreshRequired()
		}
		return SpaceCredit{}, NewStatusError(resp.StatusCode, resp.Body, resp.Headers)
	}
	var credit SpaceCredit
	var required struct {
		Balance *int64 `json:"balance"`
		Funded  *int64 `json:"funded"`
		Used    *int64 `json:"used"`
	}
	if json.Unmarshal(resp.Body, &credit) != nil || json.Unmarshal(resp.Body, &required) != nil ||
		credit.NodeID != node || required.Balance == nil || required.Funded == nil || required.Used == nil ||
		credit.Balance < 0 || credit.Funded < 0 || credit.Used < 0 || credit.Me.Used < 0 ||
		(credit.Me.Cap != nil && *credit.Me.Cap < 0) || (credit.Me.Remaining != nil && *credit.Me.Remaining < 0) ||
		(credit.Mode != "space" && credit.Mode != "personal") {
		return SpaceCredit{}, spaceError(http.StatusBadGateway, "space_credit_invalid", "Mirasim returned an invalid or mismatched space credit response")
	}
	return credit, nil
}

func (c *Client) fetchSpaceQuota(ctx context.Context, client pluginapi.HostHTTPClient) (QuotaSnapshot, error) {
	credit, err := c.FetchSpaceCredit(ctx, client)
	if err != nil {
		return QuotaSnapshot{}, err
	}
	status := "allowed"
	if credit.Mode != "space" || credit.Me.Payer != "space" || credit.Balance == 0 {
		status = "limit_reached"
	}
	quota := QuotaSnapshot{Available: true, Source: "GET /v2/machines/{nodeId}/credit", Status: status, ObservedAt: time.Now().UTC(), Space: credit.Clone()}
	c.replaceQuota(quota)
	return quota.Clone(), nil
}

func (c *Client) checkSpacePayer(ctx context.Context, client pluginapi.HostHTTPClient) error {
	credit, err := c.FetchSpaceCredit(ctx, client)
	if err != nil {
		return err
	}
	if credit.Mode != "space" || credit.Me.Payer != "space" || credit.Balance == 0 || credit.Me.Prefer == "personal" ||
		(credit.Me.Remaining != nil && *credit.Me.Remaining == 0) {
		return spaceError(http.StatusPaymentRequired, "space_credit_unavailable", "Space credit is disabled, exhausted, or not selected for this member; personal relay fallback is disabled")
	}
	return nil
}

type spaceCapabilityFile struct {
	NodeID string `json:"nodeId"`
	Token  string `json:"token"`
}

type spaceTokenClaims struct {
	Subject string `json:"sub"`
	Expires int64  `json:"exp"`
}

func decodeSpaceClaims(token string) (spaceTokenClaims, error) {
	var claims spaceTokenClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 64<<10 || strings.ContainsAny(token, "\r\n\x00") {
		return claims, fmt.Errorf("invalid space authorization")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Subject == "" || claims.Expires <= 0 {
		return claims, fmt.Errorf("invalid space authorization claims")
	}
	return claims, nil
}

func (c *Client) installSpaceTokenLocked(node, token string) error {
	claims, err := decodeSpaceClaims(token)
	if err != nil {
		return spaceError(http.StatusBadRequest, "space_authorization_invalid", "The space authorization must be a Mirasim-issued JWT with sub and exp claims")
	}
	login, err := decodeSpaceClaims(c.accessToken)
	if err != nil || claims.Subject != login.Subject {
		return spaceError(http.StatusForbidden, "space_authorization_mismatch", "The space authorization belongs to a different OAuth account")
	}
	c.spaceToken = token
	c.spaceTokenExpiresAt = time.Unix(claims.Expires, 0)
	c.storage.SpaceNodeID = node
	c.storage.SpaceCapability = token
	return nil
}

// The initial capability must come from an authorized Mirasim integration.
// Reading a supplied file is deliberate; the plugin never scavenges another
// application's process memory, sessions, or credential cache for a token.
func (c *Client) spaceCapabilityLocked(ctx context.Context, force bool) (string, error) {
	node, err := c.spaceNodeLocked()
	if err != nil {
		return "", err
	}
	if err = c.loadLocked(); err != nil {
		return "", err
	}
	if c.spaceToken == "" {
		token := c.storage.SpaceCapability
		if file := strings.TrimSpace(c.options.SpaceCapabilityFile); file != "" && (token == "" || c.storage.SpaceNodeID != node) {
			f, errOpen := os.Open(file)
			if errOpen != nil {
				return "", spaceError(http.StatusServiceUnavailable, "space_authorization_missing", "Cannot read space-capability-file")
			}
			raw, errRead := io.ReadAll(io.LimitReader(f, (128<<10)+1))
			_ = f.Close()
			var supplied spaceCapabilityFile
			if errRead != nil || len(raw) > 128<<10 || json.Unmarshal(raw, &supplied) != nil || supplied.NodeID != node {
				return "", spaceError(http.StatusBadRequest, "space_authorization_invalid", "space-capability-file must contain the selected nodeId and its authorized token")
			}
			token = strings.TrimSpace(supplied.Token)
		} else if token != "" && c.storage.SpaceNodeID != node {
			return "", spaceError(http.StatusBadRequest, "space_authorization_mismatch", "Stored space authorization does not match space-node-id")
		}
		if token == "" {
			return "", spaceError(http.StatusServiceUnavailable, "space_authorization_missing", "Space inference requires a Mirasim-issued turn capability; supply space-capability-file or space_capability in this OAuth credential")
		}
		if err = c.installSpaceTokenLocked(node, token); err != nil {
			return "", err
		}
	}
	if !force && c.nowTime().Before(c.spaceTokenExpiresAt.Add(-spaceRefreshLead)) {
		return c.spaceToken, nil
	}
	endpoint, _, err := c.endpoint(spaceRenewPath, nil)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.spaceToken)
	private, closeClient, err := newPrivateHTTPClient(c.authProxyURL, refreshTimeoutForSpace)
	if err != nil {
		return "", err
	}
	defer closeClient()
	resp, err := private.Do(req)
	if err != nil {
		return "", spaceError(http.StatusBadGateway, "space_authorization_renewal", "Cannot renew Mirasim space authorization")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", spaceError(resp.StatusCode, "space_authorization_renewal", "Mirasim refused space authorization renewal; obtain a new authorized capability")
	}
	var renewed struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(&renewed) != nil || renewed.Token == "" {
		return "", spaceError(http.StatusBadGateway, "space_authorization_renewal", "Mirasim returned an invalid space authorization renewal")
	}
	claims, err := decodeSpaceClaims(renewed.Token)
	if err != nil || !time.Unix(claims.Expires, 0).After(c.nowTime().Add(spaceRefreshLead)) {
		return "", spaceError(http.StatusBadGateway, "space_authorization_renewal", "Mirasim returned an expired space authorization renewal")
	}
	if err := c.installSpaceTokenLocked(node, renewed.Token); err != nil {
		return "", err
	}
	return c.spaceToken, nil
}

const refreshTimeoutForSpace = 30 * time.Second

func (c *Client) RefreshSpaceAuth(ctx context.Context) error {
	if !c.SpaceEnabled() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.spaceCapabilityLocked(ctx, false)
	return err
}

func (c *Client) spaceRequest(ctx context.Context, method, path string, query url.Values, headers http.Header, body []byte, stream, force bool) (pluginapi.HTTPRequest, error) {
	if !validSpaceRequestPath(path) || method != http.MethodPost {
		return pluginapi.HTTPRequest{}, spaceError(http.StatusBadRequest, "space_route_invalid", "Space gateway requests must use a supported POST model endpoint")
	}
	c.mu.Lock()
	token, err := c.spaceCapabilityLocked(ctx, force)
	var meta map[string]string
	if err == nil {
		meta, err = c.relayMetadataLocked(ctx, path, body)
	}
	c.mu.Unlock()
	if err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	endpoint, _, err := c.endpoint(spaceGatewayPath+"/"+strings.TrimLeft(path, "/"), query)
	if err != nil {
		return pluginapi.HTTPRequest{}, err
	}
	auth := http.Header{"Authorization": {"Bearer " + token}}
	for key, value := range meta {
		if key != "x-mirasim-account" {
			auth.Set(key, value)
		}
	}
	auth.Set(headerMirasimClient, c.storage.ClientVersion)
	return pluginapi.HTTPRequest{Method: method, URL: endpoint, Headers: prepareHeaders(headers, auth, stream), Body: bytes.Clone(body), WireProfile: c.options.wireProfile()}, nil
}

func validSpaceRequestPath(requestPath string) bool {
	if path.Clean(requestPath) != requestPath {
		return false
	}
	switch requestPath {
	case "/v1/messages", "/v1/messages/count_tokens", "/v1/responses", "/v1/responses/compact",
		"/v1/images/generations", "/v1/images/edits", "/v1/alpha/search":
		return true
	}
	return false
}

func confirmSpacePayer(status int, headers http.Header) error {
	if status >= 200 && status < 300 && !strings.EqualFold(strings.TrimSpace(headers.Get(spacePayerHeader)), "space") {
		return spaceError(http.StatusConflict, "space_billing_unconfirmed", "The gateway did not confirm space billing in x-mirasim-payer; inspect the space usage before retrying")
	}
	return nil
}

func (c *Client) doSpace(ctx context.Context, client pluginapi.HostHTTPClient, method, path string, query url.Values, headers http.Header, body []byte) (pluginapi.HTTPResponse, error) {
	if err := c.checkSpacePayer(ctx, client); err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := c.spaceRequest(ctx, method, path, query, headers, body, false, attempt > 0)
		if err != nil {
			return pluginapi.HTTPResponse{}, err
		}
		resp, err := client.Do(ctx, req)
		if err != nil {
			return pluginapi.HTTPResponse{}, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		if err := confirmSpacePayer(resp.StatusCode, resp.Headers); err != nil {
			return pluginapi.HTTPResponse{}, err
		}
		return resp, nil
	}
	return pluginapi.HTTPResponse{}, fmt.Errorf("space gateway retry exhausted")
}

func (c *Client) doSpaceStream(ctx context.Context, client pluginapi.HostHTTPClient, method, path string, query url.Values, headers http.Header, body []byte) (pluginapi.HTTPStreamResponse, error) {
	if err := c.checkSpacePayer(ctx, client); err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := c.spaceRequest(ctx, method, path, query, headers, body, true, attempt > 0)
		if err != nil {
			return pluginapi.HTTPStreamResponse{}, err
		}
		streamCtx, cancel := context.WithCancel(ctx)
		resp, err := client.DoStream(streamCtx, req)
		if err != nil {
			cancel()
			return pluginapi.HTTPStreamResponse{}, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			cancel()
			continue
		}
		if err := confirmSpacePayer(resp.StatusCode, resp.Headers); err != nil {
			cancel()
			return pluginapi.HTTPStreamResponse{}, err
		}
		chunks := make(chan pluginapi.HTTPStreamChunk)
		upstream := resp.Chunks
		go func() {
			defer close(chunks)
			defer cancel()
			if upstream == nil {
				return
			}
			for {
				select {
				case <-streamCtx.Done():
					return
				case chunk, ok := <-upstream:
					if !ok {
						return
					}
					select {
					case chunks <- chunk:
					case <-streamCtx.Done():
						return
					}
				}
			}
		}()
		resp.Chunks = chunks
		return resp, nil
	}
	return pluginapi.HTTPStreamResponse{}, fmt.Errorf("space gateway retry exhausted")
}
