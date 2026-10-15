package mirasim

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const spaceCreditFixture = `{"nodeId":"pc-test","mode":"space","balance":4801,"funded":4875,"used":74,"capMode":"none","me":{"cap":null,"used":65,"remaining":null,"prefer":"space","payer":"space"}}`

func spaceJWT(subject string, expiry time.Time, kind string) string {
	raw, _ := json.Marshal(map[string]any{"sub": subject, "exp": expiry.Unix(), "token_type": kind})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}

func newSpaceTestClient(t *testing.T) *Client {
	t.Helper()
	storage, _, _ := newTestStorage(t, spaceJWT("usr-test", time.Now().Add(time.Hour), "access"))
	storage.SpaceNodeID = "pc-test"
	storage.SpaceCapability = spaceJWT("usr-test", time.Now().Add(time.Hour), "turn")
	return NewPool(RelayOptions{SpaceNodeID: "pc-test"}).Client(storage)
}

func spaceCreditResponse() pluginapi.HTTPResponse {
	return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(spaceCreditFixture)}
}

func TestSpaceRoutesInferenceWithoutPersonalSigningOrCredentialLeak(t *testing.T) {
	c := newSpaceTestClient(t)
	var got []string
	host := fakeHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		u, _ := url.Parse(req.URL)
		got = append(got, u.Path)
		switch u.Path {
		case "/v2/machines/pc-test/credit":
			if req.Headers.Get("Authorization") != "Bearer "+c.storage.AccessToken {
				t.Fatal("credit query did not use OAuth")
			}
			return spaceCreditResponse(), nil
		case spaceGatewayPath + "/v1/responses":
			if req.Headers.Get("Authorization") != "Bearer "+c.storage.SpaceCapability {
				t.Fatal("inference did not use the space capability")
			}
			if u.Query().Get("test") != "1" || string(req.Body) != `{"model":"gpt-test"}` {
				t.Fatal("query or body lost")
			}
			for _, header := range []string{headerMirasimSignature, headerMirasimEncryptedMetadata, "X-Mirasim-Account", "X-Mirasim-Payer", "X-Api-Key"} {
				if req.Headers.Get(header) != "" {
					t.Fatalf("unexpected header %s", header)
				}
			}
			if req.Headers.Get(headerMirasimAgent) != "codex" || req.Headers.Get(headerMirasimCall) == "" {
				t.Fatal("gateway request metadata missing")
			}
			return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{spacePayerHeader: {"space"}}, Body: []byte(`{"ok":true}`)}, nil
		default:
			t.Fatalf("unexpected personal relay request: %s", u.Path)
			return pluginapi.HTTPResponse{}, nil
		}
	}}
	resp, err := c.Do(context.Background(), host, "POST", "/v1/responses", url.Values{"test": {"1"}}, http.Header{
		"Authorization": {"downstream-secret"}, "X-Api-Key": {"downstream-key"}, "X-Mirasim-Payer": {"personal"}, "X-Mirasim-Account": {"forged"},
	}, []byte(`{"model":"gpt-test"}`))
	if err != nil || resp.StatusCode != 200 || len(got) != 2 {
		t.Fatalf("response=%d paths=%v error=%v", resp.StatusCode, got, err)
	}
}

func TestSpacePreflightRefusesPersonalBillingWithoutInference(t *testing.T) {
	for _, change := range []string{"disabled", "personal", "empty", "member-cap", "unselected"} {
		t.Run(change, func(t *testing.T) {
			c := newSpaceTestClient(t)
			var credit SpaceCredit
			_ = json.Unmarshal([]byte(spaceCreditFixture), &credit)
			switch change {
			case "disabled":
				credit.Mode = "personal"
			case "personal":
				credit.Me.Payer = "personal"
			case "empty":
				credit.Balance = 0
			case "member-cap":
				zero := int64(0)
				credit.Me.Remaining = &zero
			case "unselected":
				credit.Me.Prefer = "personal"
			}
			raw, _ := json.Marshal(credit)
			calls := 0
			host := fakeHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				calls++
				if !strings.HasSuffix(req.URL, "/credit") {
					t.Fatal("inference reached gateway")
				}
				return pluginapi.HTTPResponse{StatusCode: 200, Body: raw}, nil
			}}
			_, err := c.Do(context.Background(), host, "POST", "/v1/messages", nil, nil, nil)
			if status, ok := err.(interface{ StatusCode() int }); !ok || status.StatusCode() != 402 || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestSpaceSuccessRequiresConfirmedPayerWithoutRetry(t *testing.T) {
	for _, payer := range []string{"", "personal"} {
		t.Run(payer, func(t *testing.T) {
			c := newSpaceTestClient(t)
			calls := 0
			host := fakeHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if strings.HasSuffix(req.URL, "/credit") {
					return spaceCreditResponse(), nil
				}
				calls++
				return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{spacePayerHeader: {payer}}}, nil
			}}
			_, err := c.Do(context.Background(), host, "POST", "/v1/messages", nil, nil, nil)
			if err == nil || calls != 1 || !strings.Contains(err.Error(), "space_billing_unconfirmed") {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestSpaceCreditValidationAndSnapshotIsolation(t *testing.T) {
	c := newSpaceTestClient(t)
	for _, raw := range []string{`{}`, strings.Replace(spaceCreditFixture, `"pc-test"`, `"another-space"`, 1), strings.Replace(spaceCreditFixture, `4801`, `-1`, 1), strings.Replace(spaceCreditFixture, `4801`, `null`, 1)} {
		host := fakeHostClient{do: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(raw)}, nil
		}}
		if _, err := c.FetchQuota(context.Background(), host); err == nil {
			t.Fatal("invalid credit accepted")
		}
	}
	host := fakeHostClient{do: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return spaceCreditResponse(), nil
	}}
	q, err := c.FetchQuota(context.Background(), host)
	if err != nil || q.Space == nil || q.Space.Balance != 4801 || len(q.Windows) != 0 {
		t.Fatalf("error=%v", err)
	}
	q.Space.Balance = 0
	if c.LastQuota().Space.Balance != 4801 {
		t.Fatal("caller mutated cached space credit")
	}
}

func TestSpaceCapabilityFilesAreBoundAndDoNotLeakSecrets(t *testing.T) {
	for _, kind := range []string{"valid", "wrong-node", "wrong-user", "invalid", "missing"} {
		t.Run(kind, func(t *testing.T) {
			c := newSpaceTestClient(t)
			c.storage.SpaceCapability = ""
			c.options.SpaceCapabilityFile = filepath.Join(t.TempDir(), "space.json")
			supplied := spaceCapabilityFile{NodeID: "pc-test", Token: spaceJWT("usr-test", time.Now().Add(time.Hour), "turn")}
			if kind == "wrong-node" {
				supplied.NodeID = "pc-other"
			}
			if kind == "wrong-user" {
				supplied.Token = spaceJWT("someone-else", time.Now().Add(time.Hour), "turn")
			}
			if kind == "invalid" {
				supplied.Token = "a-private-secret-that-must-not-appear-in-errors"
			}
			raw, _ := json.Marshal(supplied)
			if kind != "missing" {
				if err := os.WriteFile(c.options.SpaceCapabilityFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := c.RefreshSpaceAuth(context.Background())
			if kind == "valid" {
				if err != nil || c.Storage().SpaceCapability != supplied.Token {
					t.Fatal("valid authorization not installed")
				}
			} else if err == nil || strings.Contains(err.Error(), supplied.Token) {
				t.Fatal("invalid authorization accepted or secret exposed")
			}
		})
	}
}

func TestSpaceCapabilityRenewsOnceAcrossConcurrentCallsAndPersists(t *testing.T) {
	c := newSpaceTestClient(t)
	c.storage.SpaceCapability = spaceJWT("usr-test", time.Now().Add(time.Minute), "turn")
	renewed := spaceJWT("usr-test", time.Now().Add(time.Hour), "renewed-turn")
	var renewals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != spaceRenewPath || req.Method != "POST" {
			t.Error("wrong renewal route")
		}
		if req.Header.Get("Authorization") != "Bearer "+c.storage.SpaceCapability {
			t.Error("wrong renewal credential")
		}
		renewals.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": renewed})
	}))
	defer server.Close()
	c.storage.RelayURL = server.URL
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := c.RefreshSpaceAuth(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if renewals.Load() != 1 || c.Storage().SpaceCapability != renewed {
		t.Fatal("renewal was not coalesced/persisted")
	}
}

func TestSpaceUnauthorizedRenewsOnlyGatewayCredential(t *testing.T) {
	c := newSpaceTestClient(t)
	renewed := spaceJWT("usr-test", time.Now().Add(2*time.Hour), "renewed-turn")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != spaceRenewPath {
			t.Error("personal refresh attempted")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": renewed})
	}))
	defer server.Close()
	c.storage.RelayURL = server.URL
	calls := 0
	host := fakeHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, "/credit") {
			return spaceCreditResponse(), nil
		}
		calls++
		if calls == 1 {
			return pluginapi.HTTPResponse{StatusCode: 401}, nil
		}
		if req.Headers.Get("Authorization") != "Bearer "+renewed {
			t.Fatal("retry used stale authorization")
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{spacePayerHeader: {"space"}}}, nil
	}}
	if _, err := c.Do(context.Background(), host, "POST", "/v1/messages", nil, nil, nil); err != nil || calls != 2 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestSpaceStreamingUsesGatewayAndPreservesChunks(t *testing.T) {
	c := newSpaceTestClient(t)
	chunks := make(chan pluginapi.HTTPStreamChunk, 1)
	chunks <- pluginapi.HTTPStreamChunk{Payload: []byte("data: test\n\n")}
	close(chunks)
	host := fakeHostClient{
		do: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			return spaceCreditResponse(), nil
		},
		doStream: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
			if !strings.Contains(req.URL, spaceGatewayPath+"/v1/messages") {
				t.Fatal("not routed to gateway")
			}
			return pluginapi.HTTPStreamResponse{StatusCode: 200, Headers: http.Header{spacePayerHeader: {"space"}}, Chunks: chunks}, nil
		},
	}
	resp, err := c.DoStream(context.Background(), host, "POST", "/v1/messages", nil, nil, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string((<-resp.Chunks).Payload) != "data: test\n\n" {
		t.Fatal("stream chunk changed")
	}
}

func TestSpaceMisconfigurationAndMissingCapabilityNeverUsePersonalRelay(t *testing.T) {
	for _, node := range []string{"../bad", "pc-test"} {
		c := newSpaceTestClient(t)
		c.options.SpaceNodeID = node
		c.storage.SpaceCapability = ""
		host := fakeHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			if !strings.HasSuffix(req.URL, "/credit") {
				t.Fatal("unexpected inference")
			}
			return spaceCreditResponse(), nil
		}}
		if _, err := c.Do(context.Background(), host, "POST", "/v1/messages", nil, nil, nil); err == nil {
			t.Fatal("missing authorization accepted")
		}
	}
	if _, err := newSpaceTestClient(t).Do(context.Background(), nil, "POST", "/v1/messages", nil, nil, nil); err == nil {
		t.Fatal("nil client accepted")
	}
}

func TestSpaceRenewalErrorsAreRedacted(t *testing.T) {
	for _, code := range []int{401, 403, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			c := newSpaceTestClient(t)
			c.storage.SpaceCapability = spaceJWT("usr-test", time.Now(), "turn")
			secret := c.storage.SpaceCapability
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code); _, _ = w.Write([]byte(secret)) }))
			defer server.Close()
			c.storage.RelayURL = server.URL
			if err := c.RefreshSpaceAuth(context.Background()); err == nil || strings.Contains(err.Error(), secret) {
				t.Fatal("renewal error lost or secret exposed")
			}
		})
	}
}

func TestSpaceRejectedStreamCancelsUpstreamWithoutDrainingAnOpenChannel(t *testing.T) {
	c := newSpaceTestClient(t)
	var upstream context.Context
	host := fakeHostClient{
		do: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			return spaceCreditResponse(), nil
		},
		doStream: func(ctx context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
			upstream = ctx
			return pluginapi.HTTPStreamResponse{StatusCode: 200, Headers: http.Header{spacePayerHeader: {"personal"}}, Chunks: make(chan pluginapi.HTTPStreamChunk)}, nil
		},
	}
	if _, err := c.DoStream(context.Background(), host, "POST", "/v1/messages", nil, nil, nil); err == nil {
		t.Fatal("personal payer stream accepted")
	}
	if upstream == nil || upstream.Err() != context.Canceled {
		t.Fatal("rejected upstream stream was not canceled")
	}
}

func TestSpaceResetNeverRedeemsPersonalCards(t *testing.T) {
	c := newSpaceTestClient(t)
	host := fakeHostClient{do: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		t.Fatal("space reset called a personal account endpoint")
		return pluginapi.HTTPResponse{}, nil
	}}
	result, err := c.ResetQuota(context.Background(), host)
	if err != nil || result.Outcome != ResetOutcomeUnsupported {
		t.Fatal("space credit reset must be unsupported")
	}
}

func TestSpaceModelRouteCannotEscapeGatewayPrefix(t *testing.T) {
	c := newSpaceTestClient(t)
	for _, route := range []string{"/auth/refresh", "/v1/../../auth/refresh", "/v1/messages?path=other", "/v2/machines/pc-test/credit"} {
		if _, err := c.spaceRequest(context.Background(), "POST", route, nil, nil, nil, false, false); err == nil {
			t.Fatalf("non-model route accepted: %s", route)
		}
	}
}
