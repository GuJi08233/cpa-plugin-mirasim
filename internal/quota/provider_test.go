package quota

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

func percent(value float64) *float64 { return &value }

func newProvider() *Provider { return New(pluginconfig.Defaults(), mirasim.NewPool()) }

func TestDescribeQuotaClaimsOnlyMirasimAndAdvertisesReset(t *testing.T) {
	resp, errDescribe := newProvider().DescribeQuota(context.Background(), pluginapi.QuotaDescribeRequest{})
	if errDescribe != nil {
		t.Fatalf("DescribeQuota() error = %v", errDescribe)
	}
	if len(resp.SupportedProviders) != 1 || resp.SupportedProviders[0] != credentials.Provider {
		t.Fatalf("supported providers = %#v", resp.SupportedProviders)
	}
	// Mirasim grants reset cards, so a panel that hides the action would hide a
	// capability the account actually has.
	if !resp.SupportsReset {
		t.Fatal("Mirasim reset cards are redeemable and must be advertised")
	}
}

func TestResetQuotaRejectsForeignCredentials(t *testing.T) {
	_, errReset := newProvider().ResetQuota(context.Background(), pluginapi.QuotaResetRequest{
		StorageJSON: []byte(`{"type":"other"}`),
	})
	if errReset == nil || !strings.Contains(errReset.Error(), "not a Mirasim credential") {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
}

// Every outcome the relay can report has to reach the operator as a readable
// answer, and only the two accepted outcomes may clear local routing state.
func TestResetOutcomeResponseMapsEveryRelayOutcome(t *testing.T) {
	tests := []struct {
		outcome     string
		wantSuccess bool
		wantMessage string
	}{
		{mirasim.ResetOutcomeReset, true, "Redeemed a Mirasim reset card."},
		{mirasim.ResetOutcomeNothingToReset, true, "nothing to reset"},
		{mirasim.ResetOutcomeNoCard, false, "No Mirasim reset card is available"},
		{mirasim.ResetOutcomeAlreadyRedeemed, false, "already been redeemed"},
		{mirasim.ResetOutcomeExpired, false, "expired"},
		{mirasim.ResetOutcomeRevoked, false, "revoked"},
		{mirasim.ResetOutcomeUnsupported, false, "does not offer reset cards"},
		{"", false, "did not report a reset outcome"},
		{"somethingNew", false, "unrecognized reset outcome"},
	}
	for _, test := range tests {
		t.Run(test.outcome, func(t *testing.T) {
			resp := resetOutcomeResponse(mirasim.ResetCardResult{Outcome: test.outcome})
			if resp.Success != test.wantSuccess {
				t.Fatalf("success = %v, want %v (%#v)", resp.Success, test.wantSuccess, resp)
			}
			if !strings.Contains(resp.Message, test.wantMessage) {
				t.Fatalf("message = %q, want it to contain %q", resp.Message, test.wantMessage)
			}
		})
	}
}

func TestResetOutcomeResponseNamesTheClearedWindows(t *testing.T) {
	resp := resetOutcomeResponse(mirasim.ResetCardResult{
		Outcome: mirasim.ResetOutcomeReset,
		Windows: []string{"5h", "7d"},
	})
	if !resp.Success || !strings.Contains(resp.Message, "5h, 7d") {
		t.Fatalf("reset response = %#v", resp)
	}
}

func TestNormalizeSeparatesModelScopedWindows(t *testing.T) {
	resetAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	response := Normalize(credentials.Storage{Plan: "pro"}, mirasim.QuotaSnapshot{
		Windows: []mirasim.QuotaLimitWindow{
			{Name: "5h", Budget: 100, Used: 25, UsedPercent: percent(25), RemainingPercent: percent(75), ResetAt: &resetAt, Status: "allowed"},
			{Name: "7d_fable", Budget: 100, Used: 90, UsedPercent: percent(90), RemainingPercent: percent(10), ModelScoped: true, Status: "warning"},
		},
	})

	if response.Subscription == nil || response.Subscription.Plan != "pro" {
		t.Fatalf("subscription = %#v", response.Subscription)
	}
	if len(response.Groups) != 2 {
		t.Fatalf("groups = %#v", response.Groups)
	}
	if response.Groups[0].DisplayName != accountGroupName || len(response.Groups[0].Buckets) != 1 {
		t.Fatalf("account group = %#v", response.Groups[0])
	}
	if response.Groups[1].DisplayName != modelGroupName || len(response.Groups[1].Buckets) != 1 {
		t.Fatalf("model group = %#v", response.Groups[1])
	}

	account := response.Groups[0].Buckets[0]
	if account.Window != "5h" || account.RemainingFraction != 0.75 {
		t.Fatalf("account bucket = %#v", account)
	}
	if account.ResetTime != "2026-09-20T08:00:00Z" {
		t.Fatalf("reset time = %q", account.ResetTime)
	}
	if !strings.Contains(account.Description, "25.0% used") || !strings.Contains(account.Description, "allowed") {
		t.Fatalf("description = %q", account.Description)
	}

	// A model-scoped window must not reach the account summary, or one spent
	// model would read as a spent account.
	if len(response.Summary) != 1 || response.Summary[0].Key != "used_5h" || response.Summary[0].Value != 25 {
		t.Fatalf("summary = %#v", response.Summary)
	}
}

func TestNormalizeReportsNoBucketsWhenLimitsAreUnavailable(t *testing.T) {
	response := Normalize(credentials.Storage{}, mirasim.QuotaSnapshot{Source: "limits", Status: "unknown"})
	if len(response.Groups) != 0 || len(response.Summary) != 0 {
		t.Fatalf("response = %#v", response)
	}
	if response.Subscription != nil {
		t.Fatalf("subscription = %#v", response.Subscription)
	}
}

// /v1/limits reports "paid" separately from the token's plan claim, so a free
// account on a named plan has to be distinguishable from a paying one.
func TestNormalizeReportsThePaidTier(t *testing.T) {
	paid, free := true, false

	response := Normalize(credentials.Storage{Plan: "pro"}, mirasim.QuotaSnapshot{Paid: &paid})
	if response.Subscription == nil || response.Subscription.Plan != "pro" || response.Subscription.TierName != "paid" {
		t.Fatalf("paid subscription = %#v", response.Subscription)
	}

	response = Normalize(credentials.Storage{Plan: "pro"}, mirasim.QuotaSnapshot{Paid: &free})
	if response.Subscription == nil || response.Subscription.TierName != "free" {
		t.Fatalf("free subscription = %#v", response.Subscription)
	}

	// Limits that say nothing about payment must not invent a tier.
	response = Normalize(credentials.Storage{Plan: "pro"}, mirasim.QuotaSnapshot{})
	if response.Subscription == nil || response.Subscription.TierName != "" {
		t.Fatalf("unknown tier = %#v", response.Subscription)
	}

	// A paid flag with no plan claim still deserves a subscription entry.
	response = Normalize(credentials.Storage{}, mirasim.QuotaSnapshot{Paid: &paid})
	if response.Subscription == nil || response.Subscription.Plan != "" || response.Subscription.TierName != "paid" {
		t.Fatalf("planless paid subscription = %#v", response.Subscription)
	}
}

func TestNormalizeTreatsAnUnpublishedBudgetAsUnspent(t *testing.T) {
	response := Normalize(credentials.Storage{}, mirasim.QuotaSnapshot{
		Windows: []mirasim.QuotaLimitWindow{{Name: "7d", Status: "allowed"}},
	})
	if len(response.Groups) != 1 || response.Groups[0].Buckets[0].RemainingFraction != 1 {
		t.Fatalf("groups = %#v", response.Groups)
	}
	if !strings.Contains(response.Groups[0].Buckets[0].Description, "no published budget") {
		t.Fatalf("description = %q", response.Groups[0].Buckets[0].Description)
	}
}

func TestFetchQuotaRejectsForeignCredentials(t *testing.T) {
	_, errFetch := newProvider().FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{
		StorageJSON: []byte(`{"type":"other"}`),
	})
	if errFetch == nil || !strings.Contains(errFetch.Error(), "not a Mirasim credential") {
		t.Fatalf("FetchQuota() error = %v", errFetch)
	}
}
