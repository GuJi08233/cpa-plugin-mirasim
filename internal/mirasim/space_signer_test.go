package mirasim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func startTestSpaceSigner(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mirasim-sign-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "sign.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return socket
}

func writeTestSpaceProof(t *testing.T, w http.ResponseWriter, token string, changes map[string]string) {
	t.Helper()
	proof := map[string]string{"nodeId": "pc-test", "appId": "cpa", "subject": "ig-0123456789ab", "token": token,
		"authorization": "MirasimMachine v1.pc-test.123.nonce.signature"}
	for key, value := range changes {
		proof[key] = value
	}
	if err := json.NewEncoder(w).Encode(proof); err != nil {
		t.Error(err)
	}
}

func newOpaqueSignerClient(t *testing.T) (*Client, string) {
	t.Helper()
	c := newSpaceTestClient(t)
	c.options.SpaceAppID = "cpa"
	c.options.SpaceCapabilityFile = filepath.Join(t.TempDir(), "space.json")
	token := applicationOpaqueToken(t, "ig-0123456789ab", time.Now().Add(time.Hour))
	writeApplicationFile(t, c, token, "ig-0123456789ab")
	return c, token
}

func TestOpaqueInferenceBindsExactBodyHashAndGatewayPathToHostSignature(t *testing.T) {
	c, old := newOpaqueSignerClient(t)
	fresh := applicationOpaqueToken(t, "ig-0123456789ab", time.Now().Add(2*time.Hour))
	body := []byte(`{"model":"gpt-6-luna","input":"private contents","tools":[{"name":"test"}]}`)
	digest := sha256.Sum256(body)
	c.options.SpaceSignerSocket = startTestSpaceSigner(t, func(w http.ResponseWriter, req *http.Request) {
		var request map[string]string
		if req.URL.Path != "/sign" || req.Method != "POST" || json.NewDecoder(req.Body).Decode(&request) != nil {
			t.Error("invalid signer request")
		}
		if request["nodeId"] != "pc-test" || request["appId"] != "cpa" || request["path"] != spaceGatewayPath+"/v1/responses" ||
			request["method"] != "POST" || request["bodySha256"] != hex.EncodeToString(digest[:]) || len(request) != 5 {
			t.Error("signer request lost its scope or included private contents")
		}
		writeTestSpaceProof(t, w, fresh, nil)
	})
	host := fakeHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, "/credit") {
			return spaceCreditResponse(), nil
		}
		if !strings.HasPrefix(req.Headers.Get("Authorization"), "MirasimMachine v1.pc-test.") || req.Headers.Get(spaceTurnReferenceHeader) != fresh || string(req.Body) != string(body) {
			t.Error("gateway proof or body was changed")
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{spacePayerHeader: {"space"}}}, nil
	}}
	_, err := c.Do(context.Background(), host, "POST", "/v1/responses", nil, http.Header{spaceTurnReferenceHeader: {old + "forged"}}, body)
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage().SpaceCapability != fresh {
		t.Fatal("host-selected current reference was not installed")
	}
}

func TestOpaqueInferenceNeverSendsUnsignedBearerWhenSignerMissing(t *testing.T) {
	c, _ := newOpaqueSignerClient(t)
	host := fakeHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, "/credit") {
			t.Fatal("unsigned inference escaped")
		}
		return spaceCreditResponse(), nil
	}}
	_, err := c.Do(context.Background(), host, "POST", "/v1/responses", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "space_signer_missing") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHostSignerMismatchesAndFailuresAreRedacted(t *testing.T) {
	for _, scenario := range []string{"wrong-node", "wrong-app", "wrong-subject", "wrong-token", "expired", "header-injection", "bearer", "error", "unreachable"} {
		t.Run(scenario, func(t *testing.T) {
			c, token := newOpaqueSignerClient(t)
			c.options.SpaceSignerSocket = startTestSpaceSigner(t, func(w http.ResponseWriter, req *http.Request) {
				changes := map[string]string{}
				switch scenario {
				case "wrong-node":
					changes["nodeId"] = "other"
				case "wrong-app":
					changes["appId"] = "other"
				case "wrong-subject":
					changes["subject"] = "ig-aaaaaaaaaaaa"
				case "wrong-token":
					changes["token"] = applicationOpaqueToken(t, "ig-aaaaaaaaaaaa", time.Now().Add(time.Hour))
				case "expired":
					changes["token"] = applicationOpaqueToken(t, "ig-0123456789ab", time.Now().Add(-time.Hour))
				case "header-injection":
					changes["authorization"] = "MirasimMachine v1.pc-test.\r\nX-Secret: " + token
				case "bearer":
					changes["authorization"] = "Bearer " + token
				case "error":
					w.WriteHeader(403)
					_, _ = w.Write([]byte(token))
					return
				}
				writeTestSpaceProof(t, w, token, changes)
			})
			if scenario == "unreachable" {
				c.options.SpaceSignerSocket += ".missing"
			}
			if err := c.RefreshSpaceAuth(context.Background()); err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			_, err := c.spaceGatewayAuthorizationLocked(context.Background(), token, "POST", spaceRenewPath, nil)
			c.mu.Unlock()
			if err == nil || strings.Contains(err.Error(), token) {
				t.Fatal("invalid signer response accepted or credentials leaked")
			}
		})
	}
}
