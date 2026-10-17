package targetweight

// AttemptIncidentSale claims the incident's one sale back to 98% through the
// handler's gate, as the sale planner does before it plans the sale.
func AttemptIncidentSale(h *Handler, incidentID string) bool { return h.gate.AttemptSale(incidentID) }
