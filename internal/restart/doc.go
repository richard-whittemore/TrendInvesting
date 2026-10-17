// Package restart owns the rules that decide what a restart of an engine
// may start from: which earlier log is the latest run, which logs are
// abandoned attempts or abandoned starts, and whether an owner's approval
// of a fresh run restores the state that run ended in without loosening it.
//
// The state survives a restart (ADR 0019, amended 2026-09-24, "The state
// survives a restart"): leaving Degraded or Halted is never automatic, so an
// approval either clears the incident the approved log ended in, by its ID,
// or carries it in force at least as tight (CheckIncidentInForce), and the
// Boost latch is restored, never recomputed (ADR 0026, point 5;
// CheckApprovedLatch). A resume copies the old log's committed groups into
// its new log one by one, the configuration first (ADR 0027 §6), so a log
// whose committed records are a prefix of an older log's under the same
// header is a resume abandoned partway, not a run, and a log holding no
// committed record is an abandoned start (Classify).
//
// Every function here is pure over records already recovered from the
// durable logs (internal/journal); finding the logs, ordering them by
// modification time and reading them belong to the composition root.
package restart
