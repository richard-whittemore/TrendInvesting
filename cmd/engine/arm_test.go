package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// unarmable is a handler that cannot be told to await an approval.
type unarmable struct{}

func (unarmable) Apply(context.Context, event.Envelope) ([]event.Envelope, error) { return nil, nil }

// TestARequiredApprovalFailsClosedOnAHandlerThatCannotAwaitIt: when an
// approval is required and the composed handler cannot hold back its inputs
// until it is recorded, the run refuses to start rather than run unguarded,
// naming the handler.
func TestARequiredApprovalFailsClosedOnAHandlerThatCannotAwaitIt(t *testing.T) {
	err := armApproval(unarmable{}, &event.OperatorRunApprovedPayload{})
	if err == nil || !strings.Contains(err.Error(), "main.unarmable") {
		t.Fatalf("armApproval = %v, want a refusal naming the handler type main.unarmable", err)
	}
	if err := armApproval(unarmable{}, nil); err != nil {
		t.Fatalf("with no approval required armApproval = %v, want nil", err)
	}
}

// TestTheRealHandlerIsArmedByARequiredApproval: the composed target-weight
// handler refuses every input but the configuration before the approval.
func TestTheRealHandlerIsArmedByARequiredApproval(t *testing.T) {
	asOf := time.Date(2026, time.October, 16, 20, 0, 0, 0, time.UTC)
	comp, err := compose(boostConfigPath, "test-build", asOf)
	if err != nil {
		t.Fatal(err)
	}
	if err := armApproval(comp.handler, &event.OperatorRunApprovedPayload{}); err != nil {
		t.Fatalf("armApproval on the real handler = %v", err)
	}
	if _, err := comp.handler.Apply(context.Background(), comp.configEnvelope); err != nil {
		t.Fatal(err)
	}
	_, err = comp.handler.Apply(context.Background(), event.Envelope{ID: "b", Type: event.CompletedBarEventType})
	if err == nil || !strings.Contains(err.Error(), "before the owner's approval") {
		t.Fatalf("a bar before the approval: %v, want it refused", err)
	}
}
