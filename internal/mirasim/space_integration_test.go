//go:build integration

package mirasim

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	pluginconfig "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/credentials"
)

// TestLiveSpaceCredit only reads credit. It never requests a model, transfers
// funds, changes payment preferences, or refreshes the user's OAuth login.
func TestLiveSpaceCredit(t *testing.T) {
	file, node := os.Getenv("MIRASIM_SPACE_LIVE_AUTH_FILE"), os.Getenv("MIRASIM_SPACE_LIVE_NODE")
	if file == "" || node == "" {
		t.Skip("set MIRASIM_SPACE_LIVE_AUTH_FILE and MIRASIM_SPACE_LIVE_NODE for a read-only live check")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("cannot read live authorization file")
	}
	settings := pluginconfig.Defaults()
	var source struct {
		Type string `json:"type"`
		Auth struct {
			Token        string `json:"token"`
			RefreshToken string `json:"refreshToken"`
		} `json:"auth"`
	}
	if json.Unmarshal(raw, &source) != nil {
		t.Fatal("invalid authorization file")
	}
	var storage *credentials.Storage
	if source.Type == credentials.Provider {
		storage, err = credentials.Parse(raw, settings)
	} else {
		var installed credentials.Storage
		installed, err = credentials.InstallOAuth(credentials.FromSettings(settings), source.Auth.Token, source.Auth.RefreshToken)
		storage = &installed
	}
	if err != nil || storage == nil {
		t.Fatal("authorization file must be a CPA Mirasim OAuth credential or a signed-in Mirasim setting.json")
	}
	host, closeClient, err := newValidationHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := NewPool(RelayOptions{SpaceNodeID: node}).Client(*storage)
	snapshot, err := client.FetchQuota(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Available || snapshot.Space == nil || snapshot.Space.NodeID != node || len(snapshot.Windows) != 0 {
		t.Fatal("space mode returned no space credit or returned personal windows")
	}
	t.Logf("space credit read successfully: mode=%s balance=$%.2f payer=%s", snapshot.Space.Mode, float64(snapshot.Space.Balance)/100, snapshot.Space.Me.Payer)
}
