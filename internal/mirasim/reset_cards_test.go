package mirasim

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// resetCardsServer serves the device-session ticket plus the reset-card routes
// and records every path it was asked for.
type resetCardsServer struct {
	t          *testing.T
	publicKey  ed25519.PublicKey
	credential string
	listStatus int
	cards      string
	redeem     func(cardID string) (int, string)
	paths      []string
}

func (s *resetCardsServer) host() fakeHostClient {
	return fakeHostClient{do: func(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		parsed, errParse := url.Parse(req.URL)
		if errParse != nil {
			s.t.Fatalf("parse request URL: %v", errParse)
		}
		s.paths = append(s.paths, parsed.Path)
		headers := make(http.Header)
		switch {
		case parsed.Path == sessionPath:
			assertDeviceSessionRequest(s.t, s.publicKey, req, s.credential)
			return pluginapi.HTTPResponse{
				StatusCode: http.StatusOK, Headers: headers,
				Body: []byte(`{"ticket":"device-ticket","expiresIn":900}`),
			}, nil
		case parsed.Path == resetCardsPath:
			assertControlPlaneRequest(s.t, s.publicKey, req, "device-ticket")
			status := s.listStatus
			if status == 0 {
				status = http.StatusOK
			}
			return pluginapi.HTTPResponse{StatusCode: status, Headers: headers, Body: []byte(s.cards)}, nil
		case strings.HasPrefix(parsed.Path, resetCardsPath+"/") && strings.HasSuffix(parsed.Path, "/redeem"):
			assertControlPlaneRequest(s.t, s.publicKey, req, "device-ticket")
			cardID := strings.TrimSuffix(strings.TrimPrefix(parsed.Path, resetCardsPath+"/"), "/redeem")
			status, body := s.redeem(cardID)
			return pluginapi.HTTPResponse{StatusCode: status, Headers: headers, Body: []byte(body)}, nil
		default:
			return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected path %s", parsed.Path)
		}
	}}
}

func (s *resetCardsServer) redeemPaths() []string {
	redeemed := make([]string, 0, len(s.paths))
	for _, path := range s.paths {
		if strings.HasSuffix(path, "/redeem") {
			redeemed = append(redeemed, path)
		}
	}
	return redeemed
}

func newResetCardsTest(t *testing.T, server *resetCardsServer) (*Client, *resetCardsServer) {
	t.Helper()
	accessToken := futureJWT()
	storage, publicKey, _ := newTestStorage(t, accessToken)
	server.t = t
	server.publicKey = publicKey
	server.credential = accessToken
	return NewClient(storage), server
}

// The standard reset route carries no card id, so the provider has to choose.
// Spending the nearest expiry keeps longer-lived grants alive for later.
func TestResetQuotaRedeemsTheSoonestExpiringCard(t *testing.T) {
	server := &resetCardsServer{
		cards: `{"cards":[
			{"id":"card-long","status":"active","granted_at":"2026-09-01T00:00:00Z","expires_at":"2099-12-01T00:00:00Z"},
			{"id":"card-soon","status":"active","granted_at":"2026-09-01T00:00:00Z","expires_at":"2099-10-01T00:00:00Z"},
			{"id":"card-used","status":"used","granted_at":"2026-08-01T00:00:00Z","expires_at":"2099-11-01T00:00:00Z"}
		]}`,
		redeem: func(string) (int, string) {
			return http.StatusOK, `{"outcome":"reset","windows":["5h","7d"]}`
		},
	}
	client, server := newResetCardsTest(t, server)

	result, errReset := client.ResetQuota(context.Background(), server.host())
	if errReset != nil {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
	if result.Outcome != ResetOutcomeReset || result.CardID != "card-soon" {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Windows) != 2 || result.Windows[0] != "5h" || result.Windows[1] != "7d" {
		t.Fatalf("windows = %#v", result.Windows)
	}
	redeemed := server.redeemPaths()
	if len(redeemed) != 1 || redeemed[0] != resetCardsPath+"/card-soon/redeem" {
		t.Fatalf("redeemed paths = %#v", redeemed)
	}
}

// A card the relay still marks active but whose expiry has passed would be
// refused upstream, so it must not be the card that gets spent.
func TestResetQuotaSkipsAnActiveCardThatAlreadyExpired(t *testing.T) {
	server := &resetCardsServer{
		cards: `{"cards":[
			{"id":"card-stale","status":"active","granted_at":"2020-01-01T00:00:00Z","expires_at":"2020-02-01T00:00:00Z"},
			{"id":"card-live","status":"active","granted_at":"2026-09-01T00:00:00Z","expires_at":"2099-10-01T00:00:00Z"}
		]}`,
		redeem: func(string) (int, string) {
			return http.StatusOK, `{"outcome":"reset"}`
		},
	}
	client, server := newResetCardsTest(t, server)

	result, errReset := client.ResetQuota(context.Background(), server.host())
	if errReset != nil {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
	if result.CardID != "card-live" {
		t.Fatalf("redeemed %q, want the unexpired card", result.CardID)
	}
}

// With nothing to spend the provider must answer without touching the redeem
// route, because a redeem call is what consumes a grant.
func TestResetQuotaReportsNoCardWithoutRedeeming(t *testing.T) {
	server := &resetCardsServer{
		cards: `{"cards":[
			{"id":"card-used","status":"used","granted_at":"2026-08-01T00:00:00Z","expires_at":"2099-11-01T00:00:00Z"},
			{"id":"card-revoked","status":"revoked","granted_at":"2026-08-01T00:00:00Z","expires_at":"2099-11-01T00:00:00Z"}
		]}`,
		redeem: func(string) (int, string) {
			return http.StatusOK, `{"outcome":"reset"}`
		},
	}
	client, server := newResetCardsTest(t, server)

	result, errReset := client.ResetQuota(context.Background(), server.host())
	if errReset != nil {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
	if result.Outcome != ResetOutcomeNoCard {
		t.Fatalf("result = %#v", result)
	}
	if redeemed := server.redeemPaths(); len(redeemed) != 0 {
		t.Fatalf("redeemed paths = %#v", redeemed)
	}
}

// A relay that never served the route is a stable answer, not a transport
// fault, and must be distinguishable from a relay that simply has no cards.
func TestResetQuotaTreatsAMissingRouteAsUnsupported(t *testing.T) {
	server := &resetCardsServer{listStatus: http.StatusNotFound, cards: `{}`}
	client, server := newResetCardsTest(t, server)

	result, errReset := client.ResetQuota(context.Background(), server.host())
	if errReset != nil {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
	if result.Outcome != ResetOutcomeUnsupported {
		t.Fatalf("result = %#v", result)
	}
	if redeemed := server.redeemPaths(); len(redeemed) != 0 {
		t.Fatalf("redeemed paths = %#v", redeemed)
	}
}

// A card id reaches the URL as one path segment. An id carrying a separator
// could not survive re-encoding, so it is refused rather than sent to a route
// it does not name.
func TestResetQuotaRefusesACardIDThatIsNotOnePathSegment(t *testing.T) {
	server := &resetCardsServer{
		cards: `{"cards":[
			{"id":"evil/../../admin","status":"active","granted_at":"2026-09-01T00:00:00Z","expires_at":"2099-10-01T00:00:00Z"}
		]}`,
		redeem: func(string) (int, string) {
			return http.StatusOK, `{"outcome":"reset"}`
		},
	}
	client, server := newResetCardsTest(t, server)

	_, errReset := client.ResetQuota(context.Background(), server.host())
	if errReset == nil || !strings.Contains(errReset.Error(), "single URL path segment") {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
	if redeemed := server.redeemPaths(); len(redeemed) != 0 {
		t.Fatalf("redeemed paths = %#v", redeemed)
	}
}

func TestResetCardRedeemPathAcceptsRelayIssuedIDs(t *testing.T) {
	path, errPath := resetCardRedeemPath("0f8fad5b-d9cb-469f-a165-70867728950e")
	if errPath != nil {
		t.Fatalf("resetCardRedeemPath() error = %v", errPath)
	}
	if path != resetCardsPath+"/0f8fad5b-d9cb-469f-a165-70867728950e/redeem" {
		t.Fatalf("path = %q", path)
	}
	for _, bad := range []string{"", "a/b", "a%2Fb", "a b", "a?b", "a#b"} {
		if _, errBad := resetCardRedeemPath(bad); errBad == nil {
			t.Fatalf("resetCardRedeemPath(%q) accepted a value that is not one path segment", bad)
		}
	}
}

// The official client reads these instants with Date.parse, which accepts
// fractional seconds the shared quota parser rejects.
func TestParseCardInstantAcceptsFractionalSeconds(t *testing.T) {
	want := time.Date(2099, time.October, 1, 12, 0, 0, 250000000, time.UTC)
	parsed := parseCardInstant("2099-10-01T12:00:00.250Z")
	if parsed == nil || !parsed.Equal(want) {
		t.Fatalf("parsed = %#v, want %v", parsed, want)
	}
	for _, unparsable := range []string{"", "not-a-date"} {
		if got := parseCardInstant(unparsable); got != nil {
			t.Fatalf("parseCardInstant(%q) = %#v", unparsable, got)
		}
	}
}

func TestSoonestActiveResetCardPrefersTheNearestExpiry(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	instant := func(day int) *time.Time {
		value := time.Date(2099, 10, day, 0, 0, 0, 0, time.UTC)
		return &value
	}
	card, ok := soonestActiveResetCard([]ResetCard{
		{ID: "no-expiry", Status: resetCardActive},
		{ID: "later", Status: resetCardActive, ExpiresAt: instant(20)},
		{ID: "sooner", Status: resetCardActive, ExpiresAt: instant(10)},
		{ID: "used", Status: resetCardUsed, ExpiresAt: instant(5)},
	}, now)
	if !ok || card.ID != "sooner" {
		t.Fatalf("card = %#v, ok = %v", card, ok)
	}

	// With no dated card left the undated grant is still spendable.
	card, ok = soonestActiveResetCard([]ResetCard{{ID: "no-expiry", Status: resetCardActive}}, now)
	if !ok || card.ID != "no-expiry" {
		t.Fatalf("card = %#v, ok = %v", card, ok)
	}

	if _, ok := soonestActiveResetCard(nil, now); ok {
		t.Fatal("an empty card list must not yield a card")
	}
}
