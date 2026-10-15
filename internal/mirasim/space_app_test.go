package mirasim

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func applicationJWT(t *testing.T, expires time.Time, change func(*spaceTokenClaims)) string {
	t.Helper()
	claims := spaceTokenClaims{Subject: "ig-0123456789ab", Type: "gateway-turn", Audience: "pc-test",
		Expires: expires.Unix(), PaySpace: "pc-test", PayAs: "app:cpa", PayOnly: true}
	claims.App.Node, claims.App.ID, claims.App.Integration = "pc-test", "cpa", "ig_0123456789ab"
	if change != nil {
		change(&claims)
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA"}`)) + "." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}

func TestApplicationCapabilityRequiresExplicitApplicationAndSpaceOnlyScope(t *testing.T) {
	cases := map[string]func(*spaceTokenClaims){
		"valid":               nil,
		"wrong-audience":      func(c *spaceTokenClaims) { c.Audience = "another-space" },
		"array-audience":      func(c *spaceTokenClaims) { c.Audience = []string{"pc-test"} },
		"wrong-node":          func(c *spaceTokenClaims) { c.App.Node = "another-space" },
		"wrong-app":           func(c *spaceTokenClaims) { c.App.ID = "another-app" },
		"wrong-subject":       func(c *spaceTokenClaims) { c.Subject = "usr-test" },
		"missing-integration": func(c *spaceTokenClaims) { c.App.Integration = "" },
		"personal-fallback":   func(c *spaceTokenClaims) { c.PayOnly = false },
		"wrong-payer":         func(c *spaceTokenClaims) { c.PaySpace = "another-space" },
		"wrong-attribution":   func(c *spaceTokenClaims) { c.PayAs = "personal" },
		"wrong-token-kind":    func(c *spaceTokenClaims) { c.Type = "access" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := newSpaceTestClient(t)
			c.options.SpaceAppID = "cpa"
			c.storage.SpaceCapability = applicationJWT(t, time.Now().Add(time.Hour), change)
			err := c.RefreshSpaceAuth(context.Background())
			if (err == nil) != (name == "valid") {
				t.Fatalf("unexpected authorization result: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), c.storage.SpaceCapability) {
				t.Fatal("capability leaked in error")
			}
		})
	}
	c := newSpaceTestClient(t)
	c.storage.SpaceCapability = applicationJWT(t, time.Now().Add(time.Hour), nil)
	if c.RefreshSpaceAuth(context.Background()) == nil {
		t.Fatal("application capability accepted without opt-in")
	}
}

func TestApplicationFileRotationRecoversFromExpiredStoredCapability(t *testing.T) {
	c := newSpaceTestClient(t)
	c.options.SpaceAppID = "cpa"
	c.options.SpaceCapabilityFile = filepath.Join(t.TempDir(), "space.json")
	c.storage.SpaceCapability = applicationJWT(t, time.Now().Add(-time.Hour), nil)
	fresh := applicationJWT(t, time.Now().Add(time.Hour), nil)
	write := func(token string) {
		t.Helper()
		raw, _ := json.Marshal(spaceCapabilityFile{NodeID: "pc-test", AppID: "cpa", Token: token})
		if err := os.WriteFile(c.options.SpaceCapabilityFile, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(fresh)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("rotated file should avoid network renewal")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c.storage.RelayURL = server.URL
	if err := c.RefreshSpaceAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Storage().SpaceCapability != fresh {
		t.Fatal("expired persisted token took precedence over fresh file")
	}
	newer := applicationJWT(t, time.Now().Add(2*time.Hour), nil)
	write(newer)
	c.mu.Lock()
	_, err := c.spaceCapabilityLocked(context.Background(), true)
	c.mu.Unlock()
	if err != nil || c.Storage().SpaceCapability != newer {
		t.Fatalf("file rotation was not adopted: %v", err)
	}
	// An older still-valid export must not replace a later renewed token.
	write(fresh)
	// Avoid contacting the test server by checking an ordinary refresh first.
	if err := c.RefreshSpaceAuth(context.Background()); err != nil || c.Storage().SpaceCapability != newer {
		t.Fatal("ordinary refresh downgraded to an older capability")
	}
}

func TestApplicationFileCannotChangeConfiguredApplication(t *testing.T) {
	c := newSpaceTestClient(t)
	c.options.SpaceAppID = "cpa"
	c.options.SpaceCapabilityFile = filepath.Join(t.TempDir(), "space.json")
	raw, _ := json.Marshal(spaceCapabilityFile{NodeID: "pc-test", AppID: "other-app", Token: applicationJWT(t, time.Now().Add(time.Hour), nil)})
	if err := os.WriteFile(c.options.SpaceCapabilityFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if c.RefreshSpaceAuth(context.Background()) == nil {
		t.Fatal("mismatched application file accepted")
	}
}

func TestApplicationSelectorWithoutSpaceNeverUsesPersonalDiscovery(t *testing.T) {
	c := newSpaceTestClient(t)
	c.options.SpaceNodeID = ""
	c.options.SpaceAppID = "cpa"
	c.storage.SpaceNodeID = ""
	if !c.SpaceEnabled() {
		t.Fatal("application selector silently disabled space mode")
	}
	if _, err := c.ListModels(context.Background(), nil); err == nil {
		t.Fatal("incomplete application configuration returned a personal catalog")
	}
}
