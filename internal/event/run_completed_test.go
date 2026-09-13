package event_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validRunCompleted() event.RunCompletedPayload {
	return event.RunCompletedPayload{
		CompletedAt: time.Date(2026, time.February, 27, 0, 0, 0, 0, time.UTC),
	}
}

func TestRunCompletedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.RunCompletedPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "missing completed at",
			mutate:  func(p *event.RunCompletedPayload) { p.CompletedAt = time.Time{} },
			wantErr: "completed at is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := validRunCompleted()
			if tt.mutate != nil {
				tt.mutate(&payload)
			}
			err := payload.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want one containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRunCompletedEventTypeAndSchemaVersion(t *testing.T) {
	t.Parallel()

	if event.RunCompletedEventType != "replay.run.completed" {
		t.Fatalf("RunCompletedEventType = %q", event.RunCompletedEventType)
	}
	if event.RunCompletedSchemaVersion != 1 {
		t.Fatalf("RunCompletedSchemaVersion = %d, want 1", event.RunCompletedSchemaVersion)
	}
}
