package manualaudit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayUnreachableNoticeManualRemedy(t *testing.T) {
	for _, file := range []string{"server.md", "ibkr-adapter.md", "alerts-and-dashboard.md"} {
		raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", file))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		for _, want := range []string{"outside any scheduled IBKR window", "only when an adapter stops at the end of a scheduled window", "IBKR's system status", "sudo systemctl restart ti-gateway"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks link-only remedy %q", file, want)
			}
		}
		for _, old := range []string{"For the IBKR-unreachable notice, let the scheduled window finish", "Let the scheduled window finish; the Gateway healer", "reporter can also page Gateway logged out"} {
			if strings.Contains(text, old) {
				t.Errorf("%s retains obsolete link-only notice %q", file, old)
			}
		}
	}
}
