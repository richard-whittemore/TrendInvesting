package event

import "time"

// writableTime asks the JSON encoder whether t can be recorded, enforcing
// docs/development.md's validated-payload-json invariant without duplicating
// the encoder's rules. Required timestamps also need a nonzero check.
func writableTime(t time.Time) bool {
	_, err := t.MarshalJSON()
	return err == nil
}
