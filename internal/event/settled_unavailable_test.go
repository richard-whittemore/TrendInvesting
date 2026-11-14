package event_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func TestCashSnapshotSettledUnavailable(t *testing.T) {
	for _, tc := range []struct {
		version uint32
		settled string
		valid   bool
	}{
		{7, "", true}, {6, "", false}, {4, "", false},
		{7, `,"settled_cash":0`, false}, {7, `,"settled_cash":null`, true},
		{6, `,"settled_cash":0`, true},
	} {
		raw := `{"account_id":"ira","as_of":"2025-01-02T21:00:00Z","currency":"USD","net_liquidation":1000,"positions":[],"total_cash":500`
		if tc.version != 4 {
			raw += `,"quantity_scale":10000`
		}
		raw += tc.settled + `}`
		var p event.AccountCashSnapshotPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		if err := p.ValidateSchema(tc.version); (err == nil) != tc.valid {
			t.Errorf("version %d %s: %v", tc.version, tc.settled, err)
		}
		if tc.valid && tc.version == 7 {
			if p.SchemaVersion() != 7 {
				t.Error("payload did not select version 7")
			}
			encoded, err := json.Marshal(p)
			if err != nil || strings.Contains(string(encoded), `"settled_cash"`) {
				t.Errorf("absence became a figure: %s %v", encoded, err)
			}
		}
	}
}

func TestCashSnapshotStillRequiresTotalCash(t *testing.T) {
	for _, total := range []string{"", `,"total_cash":null`} {
		var p event.AccountCashSnapshotPayload
		raw := `{"account_id":"ira","as_of":"2025-01-02T21:00:00Z","currency":"USD","net_liquidation":1000,"quantity_scale":10000,"positions":[]` + total + `}`
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		if p.ValidateSchema(7) == nil {
			t.Fatal("missing total cash became zero")
		}
	}
}

func TestCashSnapshotRejectsMalformedMoney(t *testing.T) {
	for _, raw := range []string{`{"total_cash":"invalid"}`, `{"settled_cash":"invalid"}`, `{"as_of":"invalid"}`} {
		var p event.AccountCashSnapshotPayload
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			t.Fatalf("decoded malformed money: %s", raw)
		}
	}
}
