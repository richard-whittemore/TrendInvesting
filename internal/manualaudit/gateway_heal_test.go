package manualaudit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayHealerManualStatesGatesAndInstallConsent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", "server.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{"ti-gateway-heal@.service", "gateway-heal:", "two hours", "ten minutes", "pending.json.lock", "deploy/provision.sh services", "owner's yes", "last-restart.json", "once per incident"} {
		if !strings.Contains(text, want) {
			t.Errorf("manual lacks %q", want)
		}
	}
}
