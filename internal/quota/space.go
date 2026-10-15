package quota

import (
	"fmt"
	"math"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

func creditDollars(credits int64) string { return fmt.Sprintf("$%.2f", float64(credits)/100) }

func creditFraction(remaining, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Max(0, math.Min(1, float64(remaining)/float64(total)))
}

func normalizeSpace(credit mirasim.SpaceCredit) pluginapi.QuotaFetchResponse {
	description := fmt.Sprintf("%s remaining · %s used · %s funded · mode %s · payer %s",
		creditDollars(credit.Balance), creditDollars(credit.Used), creditDollars(credit.Funded), credit.Mode, credit.Me.Payer)
	if credit.Me.Reason != "" {
		description += " · " + credit.Me.Reason
	}
	buckets := []pluginapi.QuotaBucket{{
		Window: "Space balance", RemainingFraction: creditFraction(credit.Balance, credit.Funded), Description: description,
	}}
	if credit.Me.Cap != nil {
		remaining := *credit.Me.Cap - credit.Me.Used
		if credit.Me.Remaining != nil {
			remaining = *credit.Me.Remaining
		}
		buckets = append(buckets, pluginapi.QuotaBucket{
			Window: "Member share", RemainingFraction: creditFraction(remaining, *credit.Me.Cap),
			Description: fmt.Sprintf("%s used / %s member cap", creditDollars(credit.Me.Used), creditDollars(*credit.Me.Cap)),
		})
	}
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "Space " + credit.NodeID, TierName: "space"},
		Summary: []pluginapi.QuotaMetric{
			{Key: "space_balance", Label: "Space balance", Value: float64(credit.Balance) / 100, Currency: "USD", Format: "currency"},
			{Key: "space_used", Label: "Space used", Value: float64(credit.Used) / 100, Currency: "USD", Format: "currency"},
			{Key: "space_funded", Label: "Space funded", Value: float64(credit.Funded) / 100, Currency: "USD", Format: "currency"},
		},
		Groups: []pluginapi.QuotaGroup{{DisplayName: "Space credit", Buckets: buckets}},
	}
}
