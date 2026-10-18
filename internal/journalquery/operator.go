package journalquery

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Operator lists every operator input the journals record, in record
// order: the kill switch engaged or released (ADR 0026, point 3), a fresh
// run's approval with the state it starts from (ADR 0027, amended
// 2026-10-17), and a rebalance clearance (ADR 0026, point 5), each with who
// recorded it and why, to the second.
func Operator(journals []Journal, f Filter) (Table, error) {
	if err := distinct(journals); err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"journal", "seq", "time", "account", "input", "action", "by", "detail"}}
	for _, j := range journals {
		items, err := decode(j)
		if err != nil {
			return Table{}, err
		}
		for _, it := range items {
			var row []string
			switch {
			case it.killed != nil:
				action := "released"
				if it.killed.Engaged {
					action = "engaged"
				}
				row = []string{"kill-switch", action, it.killed.Operator, it.killed.Detail}
			case it.cleared != nil:
				p := it.cleared
				row = []string{"rebalance-cleared", fmt.Sprintf("cleared %s: %s", p.DecisionID, strings.Join(p.ClientOrderIDs, ";")), p.Operator, p.Detail}
			case it.approved != nil:
				p := it.approved
				action := fmt.Sprintf("after %s, latched %t; ", p.PreviousLog, *p.Boost.Latched)
				switch in := p.IncidentInForce; {
				case p.IncidentID != "":
					action += "clears incident " + p.IncidentID
				case in != nil:
					action += fmt.Sprintf("incident %s in force, %s", in.IncidentID, in.State)
					if len(in.ScopeInstrumentIDs) > 0 {
						action += " on " + strings.Join(in.ScopeInstrumentIDs, ";")
					}
					action += fmt.Sprintf(", sale attempted %t", *in.SaleAttempted)
				default:
					action += "no incident"
				}
				row = []string{"run-approved", action, p.ApprovedBy, "-"}
			default:
				continue
			}
			if !f.keepAccount(it) || !f.keepTime(it.at) {
				continue
			}
			t.Rows = append(t.Rows, append([]string{
				it.journal, strconv.FormatUint(it.seq, 10), it.at.UTC().Format(time.RFC3339), accountCell(it.account),
			}, row...))
		}
	}
	return t, nil
}
