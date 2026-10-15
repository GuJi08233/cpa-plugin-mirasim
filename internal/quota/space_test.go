package quota

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

func TestSpaceCreditIsDisplayedAsMoneyWithoutPersonalSubscription(t *testing.T) {
	credit := mirasim.SpaceCredit{NodeID: "pc-test", Mode: "space", Balance: 4801, Funded: 4875, Used: 74}
	credit.Me.Payer = "space"
	response := Normalize(credentials.Storage{Plan: "max"}, mirasim.QuotaSnapshot{Space: &credit})
	if response.Subscription.Plan != "Space pc-test" || response.Subscription.TierName != "space" {
		t.Fatal("space credit inherited the personal subscription")
	}
	if len(response.Groups) != 1 || response.Groups[0].DisplayName != "Space credit" || len(response.Groups[0].Buckets) != 1 {
		t.Fatalf("groups=%+v", response.Groups)
	}
	bucket := response.Groups[0].Buckets[0]
	if bucket.ResetTime != "" || !strings.Contains(bucket.Description, "$48.01 remaining") || !strings.Contains(bucket.Description, "$0.74 used") || !strings.Contains(bucket.Description, "$48.75 funded") {
		t.Fatalf("bucket=%+v", bucket)
	}
	if len(response.Summary) != 3 || response.Summary[0].Currency != "USD" || response.Summary[0].Value != 48.01 {
		t.Fatalf("summary=%+v", response.Summary)
	}
}

func TestSpaceMemberCapAndZeroBalanceRemainDistinct(t *testing.T) {
	credit := mirasim.SpaceCredit{NodeID: "pc-test", Mode: "space", Balance: 1000, Funded: 2000, Used: 1000}
	cap, left := int64(200), int64(0)
	credit.Me.Cap, credit.Me.Remaining, credit.Me.Used = &cap, &left, 200
	credit.Me.Payer, credit.Me.Reason = "personal", "cap_reached"
	response := normalizeSpace(credit)
	buckets := response.Groups[0].Buckets
	if len(buckets) != 2 || buckets[0].RemainingFraction != .5 || buckets[1].RemainingFraction != 0 {
		t.Fatalf("buckets=%+v", buckets)
	}
	if !strings.Contains(buckets[0].Description, "cap_reached") {
		t.Fatal("payer reason lost")
	}
	if creditFraction(0, 0) != 0 || creditFraction(200, 100) != 1 || creditFraction(-1, 100) != 0 {
		t.Fatal("invalid remaining ratio")
	}
}

func TestSpaceModeDoesNotAdvertisePersonalResetCards(t *testing.T) {
	settings := pluginconfig.Defaults()
	settings.SpaceNodeID = "pc-test"
	p := New(settings, mirasim.NewPool(mirasim.RelayOptions{SpaceNodeID: "pc-test"}))
	response, err := p.DescribeQuota(context.Background(), pluginapi.QuotaDescribeRequest{})
	if err != nil || response.SupportsReset {
		t.Fatal("space credit must not advertise personal reset cards")
	}
}
