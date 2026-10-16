package mirasim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const spaceTurnReferenceHeader = "X-Mirasim-Turn-Reference"

// Called under c.mu. gtr tokens are host-bound references: their proof signs
// method, path#reference, node, timestamp, nonce and the exact body SHA-256.
// The host broker chooses its own current capability and signs only gateway
// routes. Neither the private key nor request contents enter this socket.
func (c *Client) spaceGatewayAuthorizationLocked(ctx context.Context, token, method, requestPath string, body []byte) (http.Header, error) {
	if !strings.HasPrefix(token, "gtr.") {
		return http.Header{"Authorization": {"Bearer " + token}}, nil
	}
	socket := strings.TrimSpace(c.options.SpaceSignerSocket)
	if socket == "" {
		return nil, spaceError(http.StatusServiceUnavailable, "space_signer_missing", "Mirasim gtr authorization requires a host machine signature; configure space-signer-socket and the host signer service")
	}
	node, err := c.spaceNodeLocked()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	payload, _ := json.Marshal(map[string]string{"nodeId": node, "appId": c.options.SpaceAppID,
		"method": method, "path": requestPath, "bodySha256": hex.EncodeToString(digest[:])})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://mirasim-space-signer/sign", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, spaceError(http.StatusServiceUnavailable, "space_signer_unavailable", "Cannot reach the configured Mirasim host signer")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, spaceError(http.StatusServiceUnavailable, "space_signer_refused", "The Mirasim host signer refused this space request; inspect the host signer service")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (128<<10)+1))
	var proof struct {
		NodeID        string `json:"nodeId"`
		AppID         string `json:"appId"`
		Subject       string `json:"subject"`
		Token         string `json:"token"`
		Authorization string `json:"authorization"`
	}
	if err != nil || len(raw) > 128<<10 || json.Unmarshal(raw, &proof) != nil || proof.NodeID != node ||
		proof.AppID != c.options.SpaceAppID || proof.Subject != c.spaceTokenSubject || !strings.HasPrefix(proof.Token, "gtr.") ||
		!strings.HasPrefix(proof.Authorization, "MirasimMachine v1."+node+".") || len(proof.Authorization) > 2048 ||
		strings.ContainsAny(proof.Authorization, "\r\n\x00") {
		return nil, spaceError(http.StatusBadGateway, "space_signer_invalid", "The host signer returned an invalid or mismatched space proof")
	}
	claims, err := decodeSpaceClaims(proof.Token)
	if err != nil || claims.Subject != proof.Subject || !time.Unix(claims.Expires, 0).After(c.nowTime().Add(30*time.Second)) {
		return nil, spaceError(http.StatusBadGateway, "space_signer_invalid", "The host signer returned an invalid or expired space reference")
	}
	if err := c.installSpaceTokenLocked(node, proof.Token); err != nil {
		return nil, err
	}
	return http.Header{"Authorization": {proof.Authorization}, spaceTurnReferenceHeader: {proof.Token}}, nil
}
