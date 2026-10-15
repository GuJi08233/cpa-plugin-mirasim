package credentials

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
)

const Provider = "mirasim"

const CurrentStorageVersion = 1

const opaqueAccessTokenLifetime = 30 * time.Minute

// ProfileRefreshInterval asks CPA to re-enter the plugin refresh lifecycle
// often enough to notice a Mirasim plan change without polling per request.
const ProfileRefreshInterval = 5 * time.Minute

// Storage is the provider-owned OAuth payload persisted by CLIProxyAPI in
// auth-dir.
type Storage struct {
	StorageVersion   int            `json:"storage_version,omitempty"`
	Type             string         `json:"type"`
	AccessToken      string         `json:"access_token,omitempty"`
	RefreshToken     string         `json:"refresh_token,omitempty"`
	Expired          string         `json:"expired,omitempty"`
	LastRefresh      string         `json:"last_refresh,omitempty"`
	AccountID        string         `json:"account_id,omitempty"`
	Email            string         `json:"email,omitempty"`
	Plan             string         `json:"plan,omitempty"`
	PlanExpiresAt    *int64         `json:"plan_exp,omitempty"`
	ProfileCheckedAt string         `json:"profile_checked_at,omitempty"`
	DevicePrivateKey string         `json:"device_private_key,omitempty"`
	RelayURL         string         `json:"relay_url,omitempty"`
	AdminURL         string         `json:"admin_url,omitempty"`
	ClientVersion    string         `json:"client_version,omitempty"`
	SpaceNodeID      string         `json:"space_node_id,omitempty"`
	SpaceCapability  string         `json:"space_capability,omitempty"`
	Raw              map[string]any `json:"-"`
}

// FromSettings returns empty OAuth storage with public provider settings.
func FromSettings(settings pluginconfig.Settings) Storage {
	storage := Storage{
		Type:          Provider,
		RelayURL:      settings.RelayURL,
		AdminURL:      settings.AdminURL,
		ClientVersion: settings.ClientVersion,
	}
	storage.applyDefaults()
	return storage
}

// Parse recognizes and validates a self-contained Mirasim OAuth auth file.
func Parse(raw []byte, defaults pluginconfig.Settings) (*Storage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var probe map[string]any
	if errUnmarshal := json.Unmarshal(raw, &probe); errUnmarshal != nil {
		return nil, fmt.Errorf("decode Mirasim auth: %w", errUnmarshal)
	}
	if !strings.EqualFold(strings.TrimSpace(stringValue(probe["type"])), Provider) {
		return nil, nil
	}
	if errMigrate := migrateStorageMap(probe); errMigrate != nil {
		return nil, errMigrate
	}
	normalizedRaw, errMarshal := json.Marshal(probe)
	if errMarshal != nil {
		return nil, fmt.Errorf("normalize Mirasim auth: %w", errMarshal)
	}
	var storage Storage
	if errUnmarshal := json.Unmarshal(normalizedRaw, &storage); errUnmarshal != nil {
		return nil, fmt.Errorf("decode Mirasim auth: %w", errUnmarshal)
	}
	storage.Raw = cloneMap(probe)
	if strings.TrimSpace(storage.RelayURL) == "" {
		storage.RelayURL = defaults.RelayURL
	}
	if strings.TrimSpace(storage.AdminURL) == "" {
		storage.AdminURL = defaults.AdminURL
	}
	// Client version describes this executable's protocol, not the account.
	// Old OAuth snapshots must not pin upgraded code to an obsolete version.
	storage.ClientVersion = defaults.ClientVersion
	storage.applyDefaults()
	storage.ensureTokenTiming(time.Now())
	storage.PopulatePlanFromAccessToken()

	if errValidate := storage.Validate(); errValidate != nil {
		return nil, errValidate
	}
	return &storage, nil
}

func (s *Storage) applyDefaults() {
	s.StorageVersion = CurrentStorageVersion
	s.Type = Provider
	s.AccessToken = strings.TrimSpace(s.AccessToken)
	s.RefreshToken = strings.TrimSpace(s.RefreshToken)
	s.Expired = normalizeTimestamp(s.Expired)
	s.LastRefresh = normalizeTimestamp(s.LastRefresh)
	s.AccountID = strings.TrimSpace(s.AccountID)
	s.Email = strings.TrimSpace(s.Email)
	s.Plan = strings.TrimSpace(s.Plan)
	s.ProfileCheckedAt = normalizeTimestamp(s.ProfileCheckedAt)
	s.DevicePrivateKey = strings.TrimSpace(s.DevicePrivateKey)
	s.RelayURL = strings.TrimRight(strings.TrimSpace(s.RelayURL), "/")
	s.AdminURL = strings.TrimRight(strings.TrimSpace(s.AdminURL), "/")
	s.ClientVersion = strings.TrimSpace(s.ClientVersion)
	s.SpaceNodeID = strings.TrimSpace(s.SpaceNodeID)
	s.SpaceCapability = strings.TrimSpace(s.SpaceCapability)
	if s.RelayURL == "" {
		s.RelayURL = pluginconfig.DefaultRelayURL
	}
	if s.AdminURL == "" {
		s.AdminURL = pluginconfig.DefaultAdminURL
	}
	if s.ClientVersion == "" {
		s.ClientVersion = pluginconfig.DefaultClientVersion
	}
}

// Validate checks the complete in-memory credential payload without touching
// the filesystem.
func (s Storage) Validate() error {
	if _, errAccess := normalizeStoredSecret("access token", s.AccessToken); errAccess != nil {
		return errAccess
	}
	if _, errRefresh := normalizeStoredSecret("refresh token", s.RefreshToken); errRefresh != nil {
		return errRefresh
	}
	if strings.TrimSpace(s.DevicePrivateKey) == "" {
		return fmt.Errorf("Mirasim device private key is missing")
	}
	if !validDeviceKey([]byte(strings.TrimSpace(s.DevicePrivateKey))) {
		return fmt.Errorf("Mirasim device private key is not a valid Ed25519 PKCS#8 PEM")
	}
	return nil
}

func (s Storage) JSON() []byte {
	out := cloneMap(s.Raw)
	if out == nil {
		out = make(map[string]any)
	}
	delete(out, "credential_dir")
	delete(out, "credential_mode")
	delete(out, "expiry")
	out["storage_version"] = CurrentStorageVersion
	out["type"] = Provider
	setOrDelete(out, "access_token", strings.TrimSpace(s.AccessToken))
	setOrDelete(out, "refresh_token", strings.TrimSpace(s.RefreshToken))
	setOrDelete(out, "expired", strings.TrimSpace(s.Expired))
	setOrDelete(out, "last_refresh", strings.TrimSpace(s.LastRefresh))
	setOrDelete(out, "account_id", strings.TrimSpace(s.AccountID))
	setOrDelete(out, "email", strings.TrimSpace(s.Email))
	setOrDelete(out, "plan", strings.TrimSpace(s.Plan))
	if s.PlanExpiresAt == nil {
		delete(out, "plan_exp")
	} else {
		out["plan_exp"] = *s.PlanExpiresAt
	}
	setOrDelete(out, "profile_checked_at", strings.TrimSpace(s.ProfileCheckedAt))
	setOrDelete(out, "device_private_key", strings.TrimSpace(s.DevicePrivateKey))
	setOrDelete(out, "relay_url", strings.TrimSpace(s.RelayURL))
	setOrDelete(out, "admin_url", strings.TrimSpace(s.AdminURL))
	setOrDelete(out, "client_version", strings.TrimSpace(s.ClientVersion))
	setOrDelete(out, "space_node_id", strings.TrimSpace(s.SpaceNodeID))
	setOrDelete(out, "space_capability", strings.TrimSpace(s.SpaceCapability))
	out["auth_kind"] = "oauth"
	raw, _ := json.Marshal(out)
	return raw
}

func (s Storage) Key() string {
	return strings.Join([]string{s.AccountID, deviceFingerprint(s.DevicePrivateKey), s.RelayURL, s.AdminURL, s.ClientVersion, s.SpaceNodeID}, "\x00")
}

func (s Storage) AuthData(id, fileName string, nextRefresh time.Time) pluginapi.AuthData {
	fileName = filepath.Base(strings.TrimSpace(fileName))
	if fileName == "" || fileName == "." {
		fileName = s.DefaultAuthFileName()
	}
	if strings.TrimSpace(id) == "" {
		id = fileName
	}
	metadata := map[string]any{
		"storage_version":          CurrentStorageVersion,
		"type":                     Provider,
		"auth_kind":                "oauth",
		"access_token":             strings.TrimSpace(s.AccessToken),
		"refresh_token":            strings.TrimSpace(s.RefreshToken),
		"expired":                  strings.TrimSpace(s.Expired),
		"last_refresh":             strings.TrimSpace(s.LastRefresh),
		"account_id":               strings.TrimSpace(s.AccountID),
		"email":                    strings.TrimSpace(s.Email),
		"plan":                     strings.TrimSpace(s.Plan),
		"profile_checked_at":       strings.TrimSpace(s.ProfileCheckedAt),
		"refresh_interval_seconds": int64(ProfileRefreshInterval / time.Second),
	}
	if s.PlanExpiresAt != nil {
		metadata["plan_exp"] = *s.PlanExpiresAt
	}
	if s.SpaceNodeID != "" {
		metadata["space_node_id"] = s.SpaceNodeID
	}
	if s.SpaceCapability != "" {
		metadata["space_capability"] = s.SpaceCapability
	}
	return pluginapi.AuthData{
		Provider:    Provider,
		ID:          id,
		FileName:    fileName,
		Label:       s.AuthLabel(),
		Prefix:      strings.TrimSpace(stringValue(s.Raw["prefix"])),
		ProxyURL:    strings.TrimSpace(stringValue(s.Raw["proxy_url"])),
		Disabled:    boolValue(s.Raw["disabled"]),
		StorageJSON: s.JSON(),
		Metadata:    metadata,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
		NextRefreshAfter: nextRefresh,
	}
}

// migrateStorageMap upgrades only self-contained CPA OAuth JSON. It never
// reads or imports the obsolete credential-directory representation.
func migrateStorageMap(values map[string]any) error {
	version, errVersion := parsedStorageVersion(values["storage_version"])
	if errVersion != nil {
		return fmt.Errorf("decode Mirasim auth storage_version: %w", errVersion)
	}
	if version > CurrentStorageVersion {
		return fmt.Errorf("unsupported Mirasim auth storage_version %d (current %d)", version, CurrentStorageVersion)
	}
	if strings.TrimSpace(stringValue(values["expired"])) == "" {
		if legacyExpiry := strings.TrimSpace(stringValue(values["expiry"])); legacyExpiry != "" {
			values["expired"] = legacyExpiry
		}
	}
	delete(values, "expiry")
	delete(values, "credential_dir")
	delete(values, "credential_mode")
	values["storage_version"] = CurrentStorageVersion
	values["auth_kind"] = "oauth"
	return nil
}

func parsedStorageVersion(value any) (int, error) {
	if value == nil {
		return 0, nil
	}
	switch typed := value.(type) {
	case float64:
		version := int(typed)
		if typed != float64(version) || version < 0 {
			return 0, fmt.Errorf("must be a non-negative integer")
		}
		return version, nil
	case json.Number:
		version, errParse := strconv.Atoi(string(typed))
		if errParse != nil || version < 0 {
			return 0, fmt.Errorf("must be a non-negative integer")
		}
		return version, nil
	case int:
		if typed < 0 {
			return 0, fmt.Errorf("must be a non-negative integer")
		}
		return typed, nil
	default:
		return 0, fmt.Errorf("must be a non-negative integer")
	}
}

// PopulateIdentityFromAccessToken copies only stable account fields from the
// token. Callers must validate the token with Mirasim before persisting or
// displaying the resulting identity.
func (s *Storage) PopulateIdentityFromAccessToken() {
	if s == nil {
		return
	}
	claims := jwtClaims(s.AccessToken)
	if strings.TrimSpace(s.AccountID) == "" {
		s.AccountID = firstClaimString(claims, "account_id", "accountId", "user_id", "userId", "sub")
	}
	if strings.TrimSpace(s.Email) == "" {
		s.Email = firstClaimString(claims, "email")
	}
	s.AccountID = strings.TrimSpace(s.AccountID)
	s.Email = strings.TrimSpace(s.Email)
}

// AccessTokenAgentAccount returns the sub-account a relay request belongs to.
// The official client keeps this distinct from the user ID and sends
// x-mirasim-account only when the signed-in identity carries one, so a token
// with no such claim contributes no header. AccountID is a local identity used
// for file naming and session derivation and is not a substitute for it.
func AccessTokenAgentAccount(token string) string {
	return firstClaimString(jwtClaims(token), "account_id", "accountId")
}

// PopulatePlanFromAccessToken seeds plan metadata for a newly installed or
// older auth record. A successful /auth/me profile check remains authoritative.
func (s *Storage) PopulatePlanFromAccessToken() {
	if s == nil || strings.TrimSpace(s.Plan) != "" {
		return
	}
	plan, expiresAt := AccessTokenPlan(s.AccessToken)
	if plan == "" {
		return
	}
	s.Plan = plan
	s.PlanExpiresAt = cloneInt64(expiresAt)
}

// AccessTokenPlan returns the Mirasim plan claims carried by a JWT. Opaque or
// malformed tokens simply have no locally observable plan.
func AccessTokenPlan(token string) (string, *int64) {
	claims := jwtClaims(token)
	plan, _ := claims["plan"].(string)
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return "", nil
	}
	var expiresAt int64
	switch value := claims["plan_exp"].(type) {
	case float64:
		if value > 0 && value == float64(int64(value)) {
			expiresAt = int64(value)
		}
	case json.Number:
		expiresAt, _ = value.Int64()
	}
	if expiresAt <= 0 {
		return plan, nil
	}
	return plan, &expiresAt
}

// RecordProfile persists the non-secret account state returned by /auth/me.
func (s *Storage) RecordProfile(email, plan string, planExpiresAt *int64, checkedAt time.Time) {
	if s == nil {
		return
	}
	if email = strings.TrimSpace(email); email != "" {
		s.Email = email
	}
	if plan = strings.TrimSpace(plan); plan != "" {
		s.Plan = plan
		s.PlanExpiresAt = cloneInt64(planExpiresAt)
	}
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}
	s.ProfileCheckedAt = checkedAt.UTC().Format(time.RFC3339)
}

// ProfileCheckTime reports the last successful /auth/me observation.
func (s Storage) ProfileCheckTime() time.Time {
	parsed, _ := parseTimestamp(s.ProfileCheckedAt)
	return parsed
}

// DefaultAuthFileName mirrors CPA's account-specific OAuth files. If Mirasim
// omits account claims, the generated device identity provides a stable,
// collision-resistant fallback for this login.
func (s Storage) DefaultAuthFileName() string {
	identity := strings.TrimSpace(s.AccountID)
	if identity == "" {
		identity = strings.ToLower(strings.TrimSpace(s.Email))
	}
	component := safeFileComponent(identity)
	if component == "" {
		component = deviceFingerprint(s.DevicePrivateKey)
	}
	return "mirasim-" + component + ".json"
}

func (s Storage) AuthLabel() string {
	if email := strings.TrimSpace(s.Email); email != "" {
		return "Mirasim (" + email + ")"
	}
	if accountID := strings.TrimSpace(s.AccountID); accountID != "" {
		return "Mirasim (" + abbreviatedIdentity(accountID) + ")"
	}
	return "Mirasim (device " + abbreviatedIdentity(deviceFingerprint(s.DevicePrivateKey)) + ")"
}

func safeFileComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, char := range value {
		allowed := (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' || char == '@'
		if allowed {
			builder.WriteRune(char)
		} else if builder.Len() > 0 && !strings.HasSuffix(builder.String(), "-") {
			builder.WriteByte('-')
		}
		if builder.Len() >= 64 {
			break
		}
	}
	return strings.Trim(builder.String(), ".-_")
}

func abbreviatedIdentity(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return nil
	}
	var claims map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if errJSON := decoder.Decode(&claims); errJSON != nil {
		return nil
	}
	return claims
}

func firstClaimString(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// RecordTokenTiming stores the conventional CPA OAuth timestamps after login
// or refresh. A standard expires_in value wins, followed by JWT exp and a
// conservative fallback for opaque access tokens.
func (s *Storage) RecordTokenTiming(accessToken string, expiresIn int64, now time.Time) {
	if s == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	s.Expired = ResolveAccessTokenExpiry(accessToken, expiresIn, now).Format(time.RFC3339)
	s.LastRefresh = now.Format(time.RFC3339)
}

// AccessTokenExpiry returns the persisted expiry, or derives one for an older
// auth record that predates explicit token timing.
func (s Storage) AccessTokenExpiry(now time.Time) time.Time {
	if parsed, ok := parseTimestamp(s.Expired); ok {
		return parsed
	}
	return ResolveAccessTokenExpiry(s.AccessToken, 0, now)
}

// ResolveAccessTokenExpiry chooses an expiry without treating an opaque token
// as immediately expired, which would otherwise cause a refresh loop.
func ResolveAccessTokenExpiry(accessToken string, expiresIn int64, now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	if expiresIn > 0 {
		return now.Add(time.Duration(expiresIn) * time.Second)
	}
	if expiry := jwtExpiry(accessToken); !expiry.IsZero() {
		return expiry.UTC()
	}
	return now.Add(opaqueAccessTokenLifetime)
}

func (s *Storage) ensureTokenTiming(now time.Time) {
	if s == nil || strings.TrimSpace(s.AccessToken) == "" {
		return
	}
	if _, ok := parseTimestamp(s.Expired); !ok {
		s.Expired = ResolveAccessTokenExpiry(s.AccessToken, 0, now).Format(time.RFC3339)
	}
}

func normalizeTimestamp(value string) string {
	if parsed, ok := parseTimestamp(value); ok {
		return parsed.Format(time.RFC3339)
	}
	return ""
}

func parseTimestamp(value string) (time.Time, bool) {
	parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if errParse != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func jwtExpiry(token string) time.Time {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return time.Time{}
	}
	var claims struct {
		ExpiresAt json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if errJSON := decoder.Decode(&claims); errJSON != nil {
		return time.Time{}
	}
	seconds, errNumber := claims.ExpiresAt.Int64()
	if errNumber != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func deviceFingerprint(keyPEM string) string {
	block, _ := pem.Decode([]byte(strings.TrimSpace(keyPEM)))
	if block != nil {
		if parsed, errParse := x509.ParsePKCS8PrivateKey(block.Bytes); errParse == nil {
			if privateKey, ok := parsed.(ed25519.PrivateKey); ok {
				if publicDER, errPublic := x509.MarshalPKIXPublicKey(privateKey.Public()); errPublic == nil {
					digest := sha256.Sum256(publicDER)
					return base64.RawURLEncoding.EncodeToString(digest[:12])
				}
			}
		}
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(keyPEM)))
	return base64.RawURLEncoding.EncodeToString(digest[:12])
}

func setOrDelete(values map[string]any, key, value string) {
	if value == "" {
		delete(values, key)
		return
	}
	values[key] = value
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}
