// Package quotapage serves the Mirasim quota page as a plugin-owned resource
// route.
//
// CPA's quota capability is what Management Center reads through its own quota
// API, but the panel only renders quota adapters it was compiled with, so the
// plugin also publishes this page. Only resource routes can be embedded in the
// panel, and those routes are GET-only and unauthenticated, so the page lives
// at an unguessable path segment rather than an operator-visible one. The
// segment is generated once per process and published through the
// authenticated plugin list that the panel itself reads, so following it is no
// easier than reading that list; it does, however, appear in request logs and
// browser history. Package scope keeps it stable across the reconfigure CPA
// runs when it applies a config, so a save does not move the URL out from
// under an open panel iframe; only a real plugin reload, a new process,
// changes it.
package quotapage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
)

const (
	routePrefix = "/quota/"

	menuLabel       = "Mirasim Quota"
	menuDescription = "Mirasim account limits as reported by GET /v1/limits."
)

// QuotaFetcher reads the normalized limits for one credential. quota.Provider
// implements it, so the page renders the same answer the quota capability
// serves instead of reading limits a second way.
type QuotaFetcher interface {
	FetchQuota(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error)
}

// HostServices are the host callbacks a resource handler may reach through the
// host_callback_id the host supplies with the request. The page only reads the
// credential list, one credential's stored JSON, and the host HTTP client.
type HostServices interface {
	ListAuth(context.Context) ([]pluginapi.HostAuthFileEntry, error)
	GetAuth(context.Context, string) (pluginapi.HostAuthGetResponse, error)
	HTTPClient() pluginapi.HostHTTPClient
}

// Page is a quota page of one process. Every Page built in that process
// shares processSegment, so the URL the panel embeds survives the reconfigure
// CPA runs on each config apply; only a real plugin reload, a new process,
// changes it.
type Page struct {
	fetcher  QuotaFetcher
	segment  string
	services ProbeServices
	probes   *probeStore
}

// processSegment is generated once for the process, not once per Page: CPA
// re-runs plugin.Build on every config apply, and a segment generated there
// would move the page's URL out from under a panel iframe that is already open.
var processSegment = strings.ToLower(rand.Text())

func New(fetcher QuotaFetcher, services ...ProbeServices) *Page {
	p := &Page{fetcher: fetcher, segment: processSegment}
	if len(services) > 0 {
		p.services = services[0]
		p.probes = newProbeStore()
	}
	return p
}

// Resource is the route declaration the host turns into a menu entry. The path
// is relative: the host resolves it under the plugin's resource prefix.
func (p *Page) Resource() pluginapi.ResourceRoute {
	return pluginapi.ResourceRoute{
		Path:        routePrefix + p.segment,
		Menu:        menuLabel,
		Description: menuDescription,
	}
}

// Owns reports whether path addresses this page's route. Only a path under
// the /quota/ prefix whose final segment is the secret is claimed, not any
// path that happens to end in the secret; the segment is compared in constant
// time because it is the access control for an unauthenticated route.
func (p *Page) Owns(path string) bool {
	if p == nil || p.segment == "" {
		return false
	}
	idx := strings.LastIndex(path, routePrefix)
	if idx < 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(path[idx+len(routePrefix):]), []byte(p.segment)) == 1
}

// Serve renders the quota page. Reading limits needs the host callbacks, so a
// request that reaches the handler without them answers 503 rather than
// claiming the account has no limits. A credential whose limits cannot be read
// renders as unavailable on an otherwise successful page; only a request that
// addresses something other than this page answers 404.
func (p *Page) Serve(ctx context.Context, req pluginapi.ManagementRequest, host HostServices) (pluginapi.ManagementResponse, error) {
	if !strings.EqualFold(req.Method, http.MethodGet) {
		return renderResponse(http.StatusNotFound, pageView{Problem: problemNotFound})
	}
	if host == nil {
		return renderResponse(http.StatusServiceUnavailable, pageView{Problem: problemNoCallbacks})
	}
	client := host.HTTPClient()
	if client == nil {
		return renderResponse(http.StatusServiceUnavailable, pageView{Problem: problemNoCallbacks})
	}
	if req.Query.Get("action") != "" {
		return p.serveProbe(ctx, req, host)
	}
	return renderResponse(http.StatusOK, p.buildView(ctx, host, client))
}

type pageView struct {
	Accounts    []accountView
	Problem     string
	Empty       bool
	ReadAt      string
	ScriptNonce string
	ProbeToken  string
}

type accountView struct {
	Number      int
	Label       string
	Plan        string
	Tier        string
	Groups      []groupView
	Unavailable bool
	Empty       bool
}

type groupView struct {
	DisplayName string
	Buckets     []bucketView
}

type bucketView struct {
	Window      string
	Remaining   string
	Reset       string
	ResetAt     string
	Description string
}

func (p *Page) buildView(ctx context.Context, host HostServices, client pluginapi.HostHTTPClient) pageView {
	view := pageView{ReadAt: time.Now().UTC().Format("2006-01-02 15:04 MST")}
	if p.probes != nil && p.services.Models != nil && p.services.Executor != nil {
		view.ProbeToken = p.probes.token
	}
	entries, errList := host.ListAuth(ctx)
	if errList != nil {
		view.Problem = problemCredentialList
		return view
	}
	for _, entry := range entries {
		if !isMirasimAuth(entry) {
			continue
		}
		view.Accounts = append(view.Accounts, p.accountView(ctx, host, client, entry, len(view.Accounts)+1))
	}
	view.Empty = len(view.Accounts) == 0
	return view
}

func (p *Page) accountView(ctx context.Context, host HostServices, client pluginapi.HostHTTPClient, entry pluginapi.HostAuthFileEntry, number int) accountView {
	account := accountView{Number: number, Label: accountLabel(entry)}
	auth, errGet := host.GetAuth(ctx, entry.AuthIndex)
	if errGet != nil || len(auth.JSON) == 0 {
		account.Unavailable = true
		return account
	}
	response, errFetch := p.fetcher.FetchQuota(ctx, pluginapi.QuotaFetchRequest{
		AuthIndex:   entry.AuthIndex,
		AuthID:      entry.ID,
		Provider:    credentials.Provider,
		StorageJSON: auth.JSON,
		HTTPClient:  client,
	})
	if errFetch != nil {
		account.Unavailable = true
		return account
	}
	if response.Subscription != nil {
		account.Plan = strings.TrimSpace(response.Subscription.Plan)
		account.Tier = strings.TrimSpace(response.Subscription.TierName)
	}
	for _, group := range response.Groups {
		if len(group.Buckets) == 0 {
			continue
		}
		rendered := groupView{DisplayName: strings.TrimSpace(group.DisplayName)}
		if rendered.DisplayName == "" {
			rendered.DisplayName = groupFallbackName
		}
		for _, bucket := range group.Buckets {
			reset, resetAt := formatReset(bucket.ResetTime)
			rendered.Buckets = append(rendered.Buckets, bucketView{
				Window:      valueOrDash(bucket.Window),
				Remaining:   formatPercent(bucket.RemainingFraction),
				Reset:       reset,
				ResetAt:     resetAt,
				Description: valueOrDash(bucket.Description),
			})
		}
		account.Groups = append(account.Groups, rendered)
	}
	account.Empty = len(account.Groups) == 0
	return account
}

// isMirasimAuth filters the host's credential list down to this plugin's own
// credentials before any credential JSON is read. The provider field is what
// the host derives from the auth's registered provider.
func isMirasimAuth(entry pluginapi.HostAuthFileEntry) bool {
	return strings.EqualFold(strings.TrimSpace(entry.Provider), credentials.Provider) ||
		strings.EqualFold(strings.TrimSpace(entry.Type), credentials.Provider)
}

// accountLabel picks the credential name the operator already sees elsewhere
// in the panel, so "Account 2" can be told apart from "Account 1" without
// reading anything out of the credential JSON: an operator-set label first,
// then the auth file name. Both are host metadata, not secrets.
func accountLabel(entry pluginapi.HostAuthFileEntry) string {
	if label := strings.TrimSpace(entry.Label); label != "" {
		return label
	}
	return strings.TrimSpace(entry.Name)
}

func valueOrDash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "—"
	}
	return value
}

func formatPercent(fraction float64) string {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	return fmt.Sprintf("%.1f%%", fraction*100)
}

// formatReset keeps UTC as a readable fallback and returns the instant for
// the browser to display in its own time zone and use for the countdown.
func formatReset(value string) (display, instant string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "—", ""
	}
	parsed, errParse := time.Parse(time.RFC3339Nano, value)
	if errParse != nil {
		return value, ""
	}
	utc := parsed.UTC()
	// JavaScript dates have millisecond precision; keep the datetime within
	// the browser's standardized ISO 8601 parsing range.
	return utc.Format("2006-01-02 15:04 UTC"), utc.Truncate(time.Millisecond).Format(time.RFC3339Nano)
}

func renderResponse(status int, view pageView) (pluginapi.ManagementResponse, error) {
	if view.ReadAt == "" {
		view.ReadAt = time.Now().UTC().Format("2006-01-02 15:04 MST")
	}
	view.ScriptNonce = rand.Text()
	var body bytes.Buffer
	if errRender := pageTemplate.Execute(&body, view); errRender != nil {
		return pluginapi.ManagementResponse{}, fmt.Errorf("render Mirasim quota page: %w", errRender)
	}
	return pluginapi.ManagementResponse{StatusCode: status, Headers: pageHeaders(view.ScriptNonce), Body: body.Bytes()}, nil
}

func pageHeaders(scriptNonce string) http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "text/html; charset=utf-8")
	headers.Set("Cache-Control", "no-store")
	headers.Set("Referrer-Policy", "no-referrer")
	headers.Set("X-Content-Type-Options", "nosniff")
	// frame-ancestors 'self' is what lets the panel embed this page in its
	// iframe; the login pages keep 'none', because nothing embeds them.
	headers.Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; style-src 'unsafe-inline'; script-src 'nonce-"+scriptNonce+"'; base-uri 'none'; frame-ancestors 'self'; form-action 'none'")
	return headers
}

const (
	problemNotFound       = "找不到此配额页面。 / This quota page does not exist."
	problemNoCallbacks    = "托管方未提供凭据回调，暂时无法读取配额。 / The host did not provide credential callbacks, so limits cannot be read right now."
	problemCredentialList = "托管方未返回凭据列表，暂时无法读取配额。 / The host did not return the credential list, so limits cannot be read right now."
	emptyAccounts         = "尚未安装 Mirasim 凭据。 / No Mirasim credential is installed yet."
	unavailableAccount    = "此账户的配额暂时无法读取。 / The limits for this account could not be read right now."
	emptyAccount          = "此账户未发布限额窗口。 / This account publishes no limit windows."
	groupFallbackName     = "Limits"
)

// The page interpolates only values that were already normalized for display —
// group names, window names, percentages, reset timestamps and descriptions.
// Credential JSON is passed to the fetcher and never rendered. The fixed
// bilingual messages are template functions so the constants above remain the
// single source the tests can assert against.
var pageTemplate = template.Must(template.New("mirasim-quota").Funcs(template.FuncMap{
	"probeScript":        func() template.JS { return template.JS(probeScript) },
	"emptyAccounts":      func() string { return emptyAccounts },
	"unavailableAccount": func() string { return unavailableAccount },
	"emptyAccount":       func() string { return emptyAccount },
}).Parse(pageHTML))

//go:embed page.html
var pageHTML string

//go:embed probe.js
var probeScript string
