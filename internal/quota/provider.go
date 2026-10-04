// Package quota reports Mirasim account limits through CPA's quota provider
// capability, so Management Center renders them from its own quota page
// instead of a plugin-specific route the panel has to be taught about.
package quota

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

const (
	accountGroupName = "Account limits"
	modelGroupName   = "Model limits"
)

type Provider struct {
	settings pluginconfig.Settings
	pool     *mirasim.Pool
}

func New(settings pluginconfig.Settings, pool *mirasim.Pool) *Provider {
	return &Provider{settings: settings, pool: pool}
}

func (p *Provider) Identifier() string { return credentials.Provider }

func (p *Provider) DescribeQuota(context.Context, pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{credentials.Provider},
		DisplayName:        "Mirasim",
		// Mirasim grants reset cards that clear an exhausted window through
		// POST /v1/reset-cards/{id}/redeem. The reset route carries no
		// parameters, so the provider spends the card that expires soonest on
		// the caller's behalf and reports which card it used.
		SupportsReset: true,
	}, nil
}

// FetchQuota reads structured limits only. A credential that cannot report them
// yields an empty answer rather than anything that would bill the account.
func (p *Provider) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	storage, errParse := credentials.Parse(req.StorageJSON, p.settings)
	if errParse != nil {
		return pluginapi.QuotaFetchResponse{}, errParse
	}
	if storage == nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("selected auth is not a Mirasim credential")
	}
	if req.HTTPClient == nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("host HTTP client is required")
	}
	snapshot, errQuota := p.pool.Client(*storage).FetchQuota(ctx, req.HTTPClient)
	if errQuota != nil {
		return pluginapi.QuotaFetchResponse{}, errQuota
	}
	return Normalize(*storage, snapshot), nil
}

// ResetQuota spends one Mirasim reset card. Every outcome the relay can report
// is an answer rather than a transport failure, so an expected refusal (no
// card, already redeemed, expired) comes back as a readable message instead of
// an error the panel would render as a raw upstream fault.
func (p *Provider) ResetQuota(ctx context.Context, req pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	storage, errParse := credentials.Parse(req.StorageJSON, p.settings)
	if errParse != nil {
		return pluginapi.QuotaResetResponse{}, errParse
	}
	if storage == nil {
		return pluginapi.QuotaResetResponse{}, fmt.Errorf("selected auth is not a Mirasim credential")
	}
	if req.HTTPClient == nil {
		return pluginapi.QuotaResetResponse{}, fmt.Errorf("host HTTP client is required")
	}
	result, errReset := p.pool.Client(*storage).ResetQuota(ctx, req.HTTPClient)
	if errReset != nil {
		return pluginapi.QuotaResetResponse{}, errReset
	}
	return resetOutcomeResponse(result), nil
}

func resetOutcomeResponse(result mirasim.ResetCardResult) pluginapi.QuotaResetResponse {
	switch result.Outcome {
	case mirasim.ResetOutcomeReset:
		message := "Redeemed a Mirasim reset card"
		if len(result.Windows) > 0 {
			message += " and cleared " + strings.Join(result.Windows, ", ")
		}
		return pluginapi.QuotaResetResponse{Success: true, Message: message + "."}
	case mirasim.ResetOutcomeNothingToReset:
		// The relay accepted the request and found nothing exhausted. Local
		// routing state is still cleared, which is what the operator asked for.
		return pluginapi.QuotaResetResponse{
			Success: true,
			Message: "Mirasim reported nothing to reset; no window was exhausted.",
		}
	case mirasim.ResetOutcomeNoCard:
		return pluginapi.QuotaResetResponse{
			Success: false,
			Message: "No Mirasim reset card is available to redeem.",
		}
	case mirasim.ResetOutcomeAlreadyRedeemed:
		return pluginapi.QuotaResetResponse{
			Success: false,
			Message: "That Mirasim reset card had already been redeemed.",
		}
	case mirasim.ResetOutcomeExpired:
		return pluginapi.QuotaResetResponse{
			Success: false,
			Message: "That Mirasim reset card expired before it could be redeemed.",
		}
	case mirasim.ResetOutcomeRevoked:
		return pluginapi.QuotaResetResponse{
			Success: false,
			Message: "That Mirasim reset card was revoked.",
		}
	case mirasim.ResetOutcomeUnsupported:
		return pluginapi.QuotaResetResponse{
			Success: false,
			Message: "This Mirasim relay does not offer reset cards for the account.",
		}
	case "":
		return pluginapi.QuotaResetResponse{
			Success: false,
			Message: "Mirasim did not report a reset outcome.",
		}
	default:
		return pluginapi.QuotaResetResponse{
			Success: false,
			Message: fmt.Sprintf("Mirasim returned an unrecognized reset outcome %q.", result.Outcome),
		}
	}
}

// Normalize converts an observed limits snapshot into the shape CPA's quota
// page renders. Model-scoped windows stay in their own group so a single
// exhausted model does not read as an exhausted account.
func Normalize(storage credentials.Storage, snapshot mirasim.QuotaSnapshot) pluginapi.QuotaFetchResponse {
	response := pluginapi.QuotaFetchResponse{}
	// /v1/limits reports whether the account is paying, which the plan claim in
	// the token does not say on its own: a named plan can still be a trial. Pass
	// it through as the tier so the page can tell the two apart.
	plan := strings.TrimSpace(storage.Plan)
	if plan != "" || snapshot.Paid != nil {
		response.Subscription = &pluginapi.QuotaSubscription{Plan: plan}
		if snapshot.Paid != nil {
			response.Subscription.TierName = "free"
			if *snapshot.Paid {
				response.Subscription.TierName = "paid"
			}
		}
	}

	account := make([]pluginapi.QuotaBucket, 0, len(snapshot.Windows))
	scoped := make([]pluginapi.QuotaBucket, 0, len(snapshot.Windows))
	for _, window := range snapshot.Windows {
		bucket := bucketFromWindow(window, snapshot.Degraded)
		if window.ModelScoped {
			scoped = append(scoped, bucket)
			continue
		}
		account = append(account, bucket)
		response.Summary = append(response.Summary, metricFromWindow(window))
	}
	if len(account) > 0 {
		response.Groups = append(response.Groups, pluginapi.QuotaGroup{DisplayName: accountGroupName, Buckets: account})
	}
	if len(scoped) > 0 {
		response.Groups = append(response.Groups, pluginapi.QuotaGroup{DisplayName: modelGroupName, Buckets: scoped})
	}
	return response
}

func bucketFromWindow(window mirasim.QuotaLimitWindow, degraded bool) pluginapi.QuotaBucket {
	bucket := pluginapi.QuotaBucket{
		Window:            window.Name,
		RemainingFraction: remainingFraction(window),
		Description:       describeWindow(window, degraded),
	}
	if window.ResetAt != nil {
		bucket.ResetTime = window.ResetAt.UTC().Format(time.RFC3339)
	}
	return bucket
}

// remainingFraction reuses the already rounded percentage rather than dividing
// again, so the page and the plugin's own reporting agree to the tenth the
// official client shows.
func remainingFraction(window mirasim.QuotaLimitWindow) float64 {
	if window.RemainingPercent == nil {
		// A window without a published budget has consumed nothing observable.
		return 1
	}
	return *window.RemainingPercent / 100
}

func describeWindow(window mirasim.QuotaLimitWindow, degraded bool) string {
	parts := make([]string, 0, 3)
	if window.UsedPercent != nil {
		parts = append(parts, fmt.Sprintf("%.1f%% used", *window.UsedPercent))
	} else {
		parts = append(parts, "no published budget")
	}
	if status := strings.TrimSpace(window.Status); status != "" {
		parts = append(parts, strings.ReplaceAll(status, "_", " "))
	}
	if degraded {
		parts = append(parts, "service degraded")
	}
	return strings.Join(parts, " · ")
}

func metricFromWindow(window mirasim.QuotaLimitWindow) pluginapi.QuotaMetric {
	metric := pluginapi.QuotaMetric{
		Key:    "used_" + window.Name,
		Label:  window.Name + " used",
		Unit:   "%",
		Format: "number",
	}
	if window.UsedPercent != nil {
		metric.Value = *window.UsedPercent
	}
	return metric
}

var _ pluginapi.QuotaProvider = (*Provider)(nil)
