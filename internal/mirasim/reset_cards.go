package mirasim

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const resetCardsPath = "/v1/reset-cards"

// Card states the relay publishes for a granted reset card.
const (
	resetCardActive  = "active"
	resetCardUsed    = "used"
	resetCardRevoked = "revoked"
	resetCardExpired = "expired"
)

// Redeem outcomes the relay reports. The official client treats the whole set
// as valid answers rather than errors, so an unexpected value must not be read
// as success.
const (
	ResetOutcomeReset           = "reset"
	ResetOutcomeNothingToReset  = "nothingToReset"
	ResetOutcomeAlreadyRedeemed = "alreadyRedeemed"
	ResetOutcomeExpired         = "expired"
	ResetOutcomeRevoked         = "revoked"
	ResetOutcomeNoCard          = "noCard"
	// ResetOutcomeUnsupported marks a relay that does not serve reset cards at
	// all, which is a stable answer rather than a transport failure.
	ResetOutcomeUnsupported = "unsupported"
)

// ResetCard is one granted reset card. The relay sends instants as ISO strings;
// a card is only usable when the relay says it is active.
type ResetCard struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	GrantedAt *time.Time `json:"granted_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// ResetCardResult is the outcome of one reset attempt.
type ResetCardResult struct {
	Outcome string
	CardID  string
	Windows []string
}

type resetCardListPayload struct {
	Cards []struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		GrantedAt string `json:"granted_at"`
		ExpiresAt string `json:"expires_at"`
		UsedAt    string `json:"used_at"`
		RevokedAt string `json:"revoked_at"`
	} `json:"cards"`
}

type resetCardRedeemPayload struct {
	Outcome string   `json:"outcome"`
	Windows []string `json:"windows"`
}

// ResetQuota redeems one available reset card and reports what the relay did.
//
// The official client lists the cards and lets the operator choose. CPA's
// standard quota reset carries no parameters, so this spends the card that
// expires soonest and leaves longer-lived grants for later: a card that lapses
// unused is worth less than one that keeps its full remaining life.
//
// This only ever calls the account routes. It never issues an inference
// request, so a reset attempt cannot be billed as usage.
func (c *Client) ResetQuota(ctx context.Context, client pluginapi.HostHTTPClient) (ResetCardResult, error) {
	cards, supported, errList := c.listResetCards(ctx, client)
	if errList != nil {
		return ResetCardResult{}, errList
	}
	if !supported {
		return ResetCardResult{Outcome: ResetOutcomeUnsupported}, nil
	}
	card, okCard := soonestActiveResetCard(cards, c.nowTime())
	if !okCard {
		return ResetCardResult{Outcome: ResetOutcomeNoCard}, nil
	}
	result, errRedeem := c.redeemResetCard(ctx, client, card.ID)
	if errRedeem != nil {
		return ResetCardResult{}, errRedeem
	}
	// The cached snapshot described the account before the reset. Keeping it
	// would let a later read report the window this call just cleared.
	if result.Outcome == ResetOutcomeReset || result.Outcome == ResetOutcomeNothingToReset {
		c.replaceQuota(QuotaSnapshot{})
	}
	result.CardID = card.ID
	return result, nil
}

// listResetCards reads the granted cards. A relay that does not serve the
// route reports supported=false, which is a stable answer rather than an error
// and is distinguishable from a relay that serves the route with no cards.
func (c *Client) listResetCards(ctx context.Context, client pluginapi.HostHTTPClient) ([]ResetCard, bool, error) {
	resp, errDo := c.doControl(ctx, client, http.MethodGet, resetCardsPath, nil, http.Header{
		"Accept": []string{"application/json"},
	}, nil, nil)
	if errDo != nil {
		return nil, false, errDo
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, true, NewStatusError(resp.StatusCode, resp.Body, resp.Headers)
	}
	var payload resetCardListPayload
	if errDecode := json.Unmarshal(resp.Body, &payload); errDecode != nil {
		return nil, true, fmt.Errorf("decode Mirasim reset cards: %w", errDecode)
	}
	cards := make([]ResetCard, 0, len(payload.Cards))
	for _, entry := range payload.Cards {
		id := strings.TrimSpace(entry.ID)
		status := strings.TrimSpace(entry.Status)
		if id == "" || !knownResetCardStatus(status) {
			continue
		}
		cards = append(cards, ResetCard{
			ID:        id,
			Status:    status,
			GrantedAt: parseCardInstant(entry.GrantedAt),
			ExpiresAt: parseCardInstant(entry.ExpiresAt),
			UsedAt:    parseCardInstant(entry.UsedAt),
			RevokedAt: parseCardInstant(entry.RevokedAt),
		})
	}
	return cards, true, nil
}

func (c *Client) redeemResetCard(ctx context.Context, client pluginapi.HostHTTPClient, cardID string) (ResetCardResult, error) {
	requestPath, errPath := resetCardRedeemPath(cardID)
	if errPath != nil {
		return ResetCardResult{}, errPath
	}
	resp, errDo := c.doControl(ctx, client, http.MethodPost, requestPath, nil, http.Header{
		"Accept": []string{"application/json"},
	}, nil, []byte("{}"))
	if errDo != nil {
		return ResetCardResult{}, errDo
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return ResetCardResult{Outcome: ResetOutcomeUnsupported}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ResetCardResult{}, NewStatusError(resp.StatusCode, resp.Body, resp.Headers)
	}
	var payload resetCardRedeemPayload
	if errDecode := json.Unmarshal(resp.Body, &payload); errDecode != nil {
		return ResetCardResult{}, fmt.Errorf("decode Mirasim reset outcome: %w", errDecode)
	}
	return ResetCardResult{
		Outcome: strings.TrimSpace(payload.Outcome),
		Windows: resetWindowNames(payload.Windows),
	}, nil
}

// soonestActiveResetCard picks the usable card closest to lapsing. Cards the
// relay still calls active but whose expiry has passed are skipped rather than
// spent on a redeem the relay would refuse.
func soonestActiveResetCard(cards []ResetCard, now time.Time) (ResetCard, bool) {
	usable := make([]ResetCard, 0, len(cards))
	for _, card := range cards {
		if card.Status != resetCardActive {
			continue
		}
		if card.ExpiresAt != nil && !card.ExpiresAt.After(now) {
			continue
		}
		usable = append(usable, card)
	}
	if len(usable) == 0 {
		return ResetCard{}, false
	}
	sort.SliceStable(usable, func(i, j int) bool {
		left, right := usable[i].ExpiresAt, usable[j].ExpiresAt
		switch {
		case left == nil && right == nil:
			return false
		case left == nil:
			return false
		case right == nil:
			return true
		default:
			return left.Before(*right)
		}
	})
	return usable[0], true
}

// parseCardInstant reads one card instant. The official client hands these to
// Date.parse, which also accepts fractional seconds and bare offsets, so a
// value the shared quota parser rejects is retried before being dropped.
func parseCardInstant(value string) *time.Time {
	if parsed := resetTime(value); parsed != nil {
		return parsed
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z0700", "2006-01-02T15:04:05Z0700"} {
		if parsed, errParse := time.Parse(layout, trimmed); errParse == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

func knownResetCardStatus(status string) bool {
	switch status {
	case resetCardActive, resetCardUsed, resetCardRevoked, resetCardExpired:
		return true
	default:
		return false
	}
}

// resetCardRedeemPath builds the redeem route for one relay-issued card id.
//
// Card ids arrive from the relay and reach the URL as a single path segment.
// The transport re-encodes the request path, so an id carrying a separator or a
// percent escape could not survive the round trip; such an id is refused here
// instead of being sent to a route it does not name.
func resetCardRedeemPath(cardID string) (string, error) {
	if cardID == "" {
		return "", fmt.Errorf("Mirasim reset card id is required")
	}
	for _, r := range cardID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '~':
		default:
			return "", fmt.Errorf("Mirasim reset card id %q is not a single URL path segment", cardID)
		}
	}
	return resetCardsPath + "/" + cardID + "/redeem", nil
}

// The relay echoes the windows a redeem cleared as plain strings.
func resetWindowNames(values []string) []string {
	names := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}
