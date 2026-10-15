package auth

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

type Provider struct {
	settings pluginconfig.Settings
	pool     *mirasim.Pool
	oauth    *oauthCoordinator
	// prompter overrides the process-wide stdin reader that serves interactive
	// login prompts. Only tests set it.
	prompter *stdinPrompter
}

const refreshTimeout = 60 * time.Second

func New(settings pluginconfig.Settings, pool *mirasim.Pool) *Provider {
	return &Provider{settings: settings, pool: pool, oauth: newOAuthCoordinator()}
}

func (p *Provider) Identifier() string { return credentials.Provider }

func (p *Provider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	storage, errParse := credentials.Parse(req.RawJSON, p.settings)
	if errParse != nil {
		return pluginapi.AuthParseResponse{Handled: true}, errParse
	}
	if storage == nil {
		return pluginapi.AuthParseResponse{}, nil
	}
	p.pool.Forget(*storage)
	client := p.pool.Client(*storage)
	if errProxy := client.SetAuthProxy(req.Host.ProxyURL); errProxy != nil {
		return pluginapi.AuthParseResponse{Handled: true}, errProxy
	}
	if errValidate := client.Validate(); errValidate != nil {
		return pluginapi.AuthParseResponse{Handled: true}, errValidate
	}
	auth := storage.AuthData(req.FileName, req.FileName, client.NextRefreshAfter(time.Now()))
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth, Auths: []pluginapi.AuthData{auth}}, nil
}

func (p *Provider) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	storage, errParse := credentials.Parse(req.StorageJSON, p.settings)
	if errParse != nil {
		return pluginapi.AuthRefreshResponse{}, errParse
	}
	if storage == nil {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("Mirasim auth storage is missing")
	}
	client := p.pool.Client(*storage)
	refreshCtx, cancelRefresh := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancelRefresh()
	if _, errRefresh := client.RefreshForHost(refreshCtx, req.Host.ProxyURL); errRefresh != nil {
		return pluginapi.AuthRefreshResponse{}, errRefresh
	}
	// OAuth may already have rotated its refresh token. A separate space
	// renewal failure must not discard that new credential before CPA saves it.
	errSpace := client.RefreshSpaceAuth(refreshCtx)
	*storage = client.Storage()
	next := client.NextRefreshAfter(time.Now())
	auth := storage.AuthData(req.AuthID, req.AuthID, next)
	if client.SpaceEnabled() {
		auth.Metadata["space_authorization_status"] = "ready"
		if errSpace != nil {
			auth.Metadata["space_authorization_status"] = "unavailable"
		}
	}
	// The refresh request does not carry the physical filename. Leave it empty
	// so the host preserves the existing auth-dir filename.
	auth.FileName = ""
	mergeAuthMetadata(&auth, req.Metadata)
	mergeAuthAttributes(&auth, req.Attributes)
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: next}, nil
}

func (p *Provider) finalizeOAuthStorage(ctx context.Context, settings pluginconfig.Settings, accessToken, refreshToken, proxyURL string, hostClient pluginapi.HostHTTPClient) (credentials.Storage, error) {
	storage, errStorage := credentials.InstallOAuth(credentials.FromSettings(settings), accessToken, refreshToken)
	if errStorage != nil {
		return credentials.Storage{}, errStorage
	}
	// Identity claims affect only naming and labels after this token has passed
	// the authenticated relay validation below.
	storage.PopulateIdentityFromAccessToken()
	client := p.pool.Client(storage)
	if errValidate := client.Validate(); errValidate != nil {
		return credentials.Storage{}, errValidate
	}
	// Mirasim's official client treats /auth/me as best-effort during login.
	// Capture its plan state when reachable, but keep the signed relay
	// validation below as the persistence gate.
	_, _ = client.RefreshForHost(ctx, proxyURL)
	if errRemote := client.ValidateRemote(ctx, hostClient, proxyURL); errRemote != nil {
		p.pool.Forget(storage)
		return credentials.Storage{}, errRemote
	}
	return client.Storage(), nil
}

func (p *Provider) RegisterCommandLine(context.Context, pluginapi.CommandLineRegistrationRequest) (pluginapi.CommandLineRegistrationResponse, error) {
	return pluginapi.CommandLineRegistrationResponse{Flags: []pluginapi.CommandLineFlag{
		{Name: "mirasim-login", Usage: "Run Mirasim browser OAuth login.", Type: "bool", DefaultValue: "false"},
		{Name: "mirasim-login-provider", Usage: "Mirasim OAuth provider ID from /auth/oauth/providers (default github).", Type: "string", DefaultValue: "github"},
		{Name: "mirasim-login-email", Usage: "Sign in with a code mailed to this Mirasim account address instead of an OAuth provider.", Type: "string"},
		{Name: "mirasim-login-code", Usage: "Mirasim sign-in code, to complete an email login without a prompt.", Type: "string"},
		{Name: "mirasim-relay-url", Usage: "Mirasim relay base URL.", Type: "string"},
		{Name: "mirasim-admin-url", Usage: "Mirasim authentication service base URL.", Type: "string"},
		{Name: "mirasim-client-version", Usage: "Value sent in x-mirasim-client.", Type: "string"},
	}}, nil
}

func (p *Provider) ExecuteCommandLine(ctx context.Context, req pluginapi.CommandLineExecutionRequest) (pluginapi.CommandLineExecutionResponse, error) {
	login := flagBool(req.TriggeredFlags, "mirasim-login")
	if !login {
		return pluginapi.CommandLineExecutionResponse{}, nil
	}
	settings := p.settingsFromFlags(req.Flags)
	var auth pluginapi.AuthData
	var stdout []byte
	var errLogin error
	if email := flagString(req.Flags, "mirasim-login-email"); email != "" {
		auth, stdout, errLogin = p.runEmailLogin(ctx, settings, email, flagString(req.Flags, "mirasim-login-code"), req.Host.ProxyURL)
	} else {
		auth, stdout, errLogin = p.runLocalLogin(ctx, settings, flagStringSet(req.Flags, "mirasim-login-provider"), req.Host.ProxyURL, flagBoolValue(req.Flags, "no-browser"))
	}
	if errLogin != nil {
		return pluginapi.CommandLineExecutionResponse{Stdout: stdout, Stderr: []byte(errLogin.Error() + "\n"), ExitCode: 1}, nil
	}
	return pluginapi.CommandLineExecutionResponse{Stdout: stdout, Auths: []pluginapi.AuthData{auth}}, nil
}

func mergeAuthMetadata(auth *pluginapi.AuthData, existing map[string]any) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	for key, value := range existing {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "access_token", "refresh_token", "space_capability", "space_authorization_status", "device_private_key", "credential_dir":
			continue
		}
		if _, present := auth.Metadata[key]; !present {
			auth.Metadata[key] = value
		}
	}
	auth.Metadata["type"] = credentials.Provider
	auth.Metadata["auth_kind"] = "oauth"
	delete(auth.Metadata, "credential_dir")
	delete(auth.Metadata, "credential_mode")
}

func mergeAuthAttributes(auth *pluginapi.AuthData, existing map[string]string) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for key, value := range existing {
		if _, present := auth.Attributes[key]; !present {
			auth.Attributes[key] = value
		}
	}
	auth.Attributes["auth_kind"] = "oauth"
}

func (p *Provider) settingsFromFlags(flags map[string]pluginapi.CommandLineFlagValue) pluginconfig.Settings {
	settings := p.settings
	if value := flagString(flags, "mirasim-relay-url"); value != "" {
		settings.RelayURL = value
	}
	if value := flagString(flags, "mirasim-admin-url"); value != "" {
		settings.AdminURL = value
	}
	if value := flagString(flags, "mirasim-client-version"); value != "" {
		settings.ClientVersion = value
	}
	return settings
}

func flagBool(flags map[string]pluginapi.CommandLineFlagValue, name string) bool {
	value, ok := flags[name]
	return ok && value.Set && strings.EqualFold(strings.TrimSpace(value.Value), "true")
}

func flagString(flags map[string]pluginapi.CommandLineFlagValue, name string) string {
	value, ok := flags[name]
	if !ok {
		return ""
	}
	return strings.TrimSpace(value.Value)
}

// flagStringSet reports a flag only when the operator actually passed it, so the
// flag's own registered default does not shadow the configured oauth-login-provider.
func flagStringSet(flags map[string]pluginapi.CommandLineFlagValue, name string) string {
	value, ok := flags[name]
	if !ok || !value.Set {
		return ""
	}
	return strings.TrimSpace(value.Value)
}
