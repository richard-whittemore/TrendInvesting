package report

import (
	"fmt"
	"strings"
	"time"
)

// KindGatewayLoggedOut is IB Gateway sitting logged out of IBKR with its unit
// still active: nothing reaches IBKR and no unit has failed to say so (ADR
// 0019, "Alerting"). It repeats hourly until the Gateway logs in again.
const KindGatewayLoggedOut Kind = "gateway-logged-out"

// GatewayKey is the incident key of a logged-out Gateway on host. The Gateway
// is the host's, shared by every account, so one key serves them all.
func GatewayKey(host string) string { return "gateway-logged-out:" + host }

// GatewayLoggedOutAlert reports a Gateway that has been logged out since the
// given time, with what the health check said, and the manual's remedy
// (docs/manual/server.md, "The Gateway is logged out"). It repeats, so the
// owner is reminded while it lasts.
func GatewayLoggedOutAlert(host string, since, now time.Time, said string) Alert {
	return Alert{
		Key: GatewayKey(host), Kind: KindGatewayLoggedOut, Severity: Urgent, AccountID: host, At: now, Repeat: true,
		Title: fmt.Sprintf("%s: IB Gateway is logged out", host),
		Body: fmt.Sprintf("IB Gateway has been logged out of IBKR since about %s (checked at %s), outside its restart windows; its unit may still show active. Nothing reaches IBKR meanwhile: every account's engine is blind and cannot trade. The check said: %s "+
			"Remedy (docs/manual/server.md, \"The Gateway is logged out\"): check the paper login on IBKR's website first (a refused login there means the password or a lock, not the Gateway); log out of any other session using the paper username. "+
			"If the check says login retries stopped, the start script has stopped the Gateway after 3 refused logins so IBKR cannot lock the login, and a restart is refused until you `sudo rm -f /var/lib/ti-gateway/login-failures`. "+
			"Then `sudo systemctl restart ti-gateway`, approve the phone prompt if it comes, and confirm `journalctl -u ti-gateway` shows Login has completed. You are reminded hourly until the Gateway is logged in; it then resolves.",
			since.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), strings.TrimSpace(said)),
	}
}
