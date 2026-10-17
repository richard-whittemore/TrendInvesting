// Package report turns a recorded journal into what a person reads: a daily
// summary per account and a list of alerts. It is a pure read of the journal's
// envelopes (ADR 0017): it never writes to an engine, reads no clock and opens
// no file, so the same journal always gives the same summary and the same
// alerts. Delivery and serving belong to cmd/.
//
// Each alert carries a Key that names its incident, so a caller that has
// already delivered a key does not deliver it again (docs/live-trading/plan.md
// section 3; ADR 0019, "Alerting").
package report
