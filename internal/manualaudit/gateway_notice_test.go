package manualaudit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The link-only notice's remedy lives in one place, the server manual's
// "Gateway healer" section; the other chapters point to it and never repeat
// advice that has gone stale (docs/manual/server.md, "Gateway healer").
func TestGatewayUnreachableNoticeManualRemedy(t *testing.T) {
	read := func(file string) string {
		raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "manual", file))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(strings.Fields(string(raw)), " ")
	}
	server := read("server.md")
	for _, want := range []string{"Inside a scheduled IBKR window", "outside a window, check IBKR's system status", "only when an adapter stops at the end of a scheduled window", "sudo systemctl restart ti-gateway"} {
		if !strings.Contains(server, want) {
			t.Errorf("server.md lacks link-only remedy %q", want)
		}
	}
	for _, file := range []string{"server.md", "ibkr-adapter.md", "alerts-and-dashboard.md"} {
		text := read(file)
		if file != "server.md" && !strings.Contains(text, "Gateway healer") {
			t.Errorf("%s does not point to the server manual's Gateway healer section", file)
		}
		for _, old := range []string{"For the IBKR-unreachable notice, let the scheduled window finish", "Let the scheduled window finish; the Gateway healer", "Let the scheduled window finish. The Gateway healer", "outside any scheduled IBKR window", "reporter can also page Gateway logged out", "The healer acts only at a scheduled window's end |"} {
			if strings.Contains(text, old) {
				t.Errorf("%s retains obsolete link-only notice %q", file, old)
			}
		}
	}
}
