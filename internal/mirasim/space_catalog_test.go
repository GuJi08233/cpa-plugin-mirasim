package mirasim

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"golang.org/x/net/websocket"
)

func spaceCatalogFixtureServer(t *testing.T, mode *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	mux.HandleFunc("/v2/gateway/session", func(w http.ResponseWriter, req *http.Request) {
		if mode.Load() == 1 {
			w.WriteHeader(503)
			return
		}
		if req.Method != "POST" || !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			t.Error("catalog session did not use OAuth POST")
		}
		var request map[string]string
		if json.NewDecoder(req.Body).Decode(&request) != nil || request["nodeId"] != "pc-test" {
			t.Error("catalog session did not select pc-test")
		}
		_ = json.NewEncoder(w).Encode(spaceGatewaySession{Session: "private-session", NodeID: "pc-test", GatewayURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/v2/gateway/ws"})
	})
	mux.Handle("/v2/gateway/ws", websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var auth struct {
			Type   string `json:"type"`
			Token  string `json:"token"`
			NodeID string `json:"nodeId"`
			Wake   int    `json:"wake"`
		}
		if websocket.JSON.Receive(ws, &auth) != nil || auth.Type != "gatewayAuth" || auth.NodeID != "pc-test" || auth.Token != "private-session" || auth.Wake != 0 {
			t.Error("invalid space gateway authorization")
			return
		}
		_ = websocket.JSON.Send(ws, map[string]string{"type": "gatewayHostCaps"})
		for range spaceCatalogAgents {
			var request map[string]string
			if websocket.JSON.Receive(ws, &request) != nil {
				return
			}
			if request["type"] != "getCatalog" {
				t.Error("space discovery sent a non-catalog operation")
				return
			}
			if mode.Load() == 2 {
				continue // Let the caller's context cancel an unresponsive space.
			}
			frame := spaceCatalogFrame{Type: "catalog", Agent: request["agent"], Models: []spaceCatalogRow{}}
			switch request["agent"] {
			case "codex":
				frame.Models = []spaceCatalogRow{{ID: "gpt-space", ContextWindow: 872000}, {ID: "kimi-code/k3", ContextWindow: 1048576}, {ID: "gpt-own"}, {ID: "profile:private-model"}, {ID: "gpt-space-paid"}}
				frame.OwnOnlyModels = []json.RawMessage{json.RawMessage(`"gpt-own"`)}
			case "claude":
				frame.Models = []spaceCatalogRow{{ID: "claude-space[1m]", Label: "Space Claude", ContextWindow: 1000000}, {ID: "kimi-k3", ContextWindow: 1000000}}
			case "pi":
				frame.Models = []spaceCatalogRow{{ID: "gemini-space", ContextWindow: 1048576}, {ID: "glm-direct"}}
				frame.RouteByModel = map[string]string{"glm-direct": "direct"}
			default:
				t.Error("unexpected agent", request["agent"])
				return
			}
			if mode.Load() == 3 {
				frame.Models = []spaceCatalogRow{}
			}
			if websocket.JSON.Send(ws, frame) != nil {
				return
			}
			if mode.Load() == 4 {
				return // A partial catalog must not replace a complete cache.
			}
		}
		if mode.Load() == 2 {
			var ignored any
			_ = websocket.JSON.Receive(ws, &ignored)
		}
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		t.Error("unexpected personal or model inference request", req.URL.Path)
		w.WriteHeader(500)
	})
	return server
}

func TestSpaceCatalogUsesSpaceConnectionWithoutTurnCapability(t *testing.T) {
	var mode atomic.Int32
	server := spaceCatalogFixtureServer(t, &mode)
	defer server.Close()
	client := newSpaceTestClient(t)
	client.storage.RelayURL = server.URL
	client.storage.SpaceCapability = ""
	host := fakeHostClient{do: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		t.Fatal("space discovery used the personal host HTTP catalog")
		return pluginapi.HTTPResponse{}, nil
	}}
	catalog, err := client.ListModels(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, model := range catalog.Models {
		ids = append(ids, model.ID)
		if model.OwnedBy != "mirasim-space" || model.MaxInputTokens == 0 {
			t.Fatal("space catalog provenance/context was lost")
		}
	}
	if !reflect.DeepEqual(ids, []string{"claude-space", "gemini-space", "gpt-space", "kimi-k3"}) {
		t.Fatalf("space models=%v", ids)
	}
	if len(client.CachedModels()) != 4 || client.ModelRoster(context.Background(), host).Models["claude-space"].ContextWindow != 1000000 {
		t.Fatal("space catalog cache or model metadata missing")
	}
	if client.Storage().SpaceCapability != "" {
		t.Fatal("model discovery manufactured an inference capability")
	}
}

func TestSpaceCatalogFailurePreservesOnlyCompleteSpaceCache(t *testing.T) {
	for _, failure := range []int32{1, 3, 4} {
		t.Run(string(rune('0'+failure)), func(t *testing.T) {
			var mode atomic.Int32
			server := spaceCatalogFixtureServer(t, &mode)
			defer server.Close()
			client := newSpaceTestClient(t)
			client.storage.RelayURL = server.URL
			first, err := client.ListModels(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			mode.Store(failure)
			if _, err := client.ListModels(context.Background(), nil); err == nil {
				t.Fatal("space catalog failure was hidden")
			}
			if !reflect.DeepEqual(first.Models, client.CachedModels()) {
				t.Fatal("partial/failed space discovery replaced the complete cache")
			}
		})
	}
}

func TestSpaceCatalogHonorsCancellation(t *testing.T) {
	var mode atomic.Int32
	mode.Store(2)
	server := spaceCatalogFixtureServer(t, &mode)
	defer server.Close()
	client := newSpaceTestClient(t)
	client.storage.RelayURL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := client.ListModels(ctx, nil); err == nil {
		t.Fatal("unresponsive space produced a catalog")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("space catalog ignored cancellation")
	}
}

func TestSpaceCatalogRejectsForeignOrInsecureGateway(t *testing.T) {
	for _, gateway := range []string{"wss://other.example/ws", "ws://relay.example/ws", "wss://user:secret@relay.example/ws"} {
		if _, err := dialSpaceCatalog(context.Background(), gateway, "https://relay.example", ""); err == nil {
			t.Fatal("unsafe gateway accepted")
		}
	}
}
