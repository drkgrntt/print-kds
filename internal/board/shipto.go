package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ShipTo is an order's destination address, stored as JSON in
// orders.shipping_address (a pre-existing free-form JSONB column - no
// migration needed). Orders that predate it hold '{}', i.e. the zero
// value, which IsEmpty reports.
type ShipTo struct {
	Name       string `json:"name"`
	Line1      string `json:"line1"`
	Line2      string `json:"line2,omitempty"`
	City       string `json:"city"`
	State      string `json:"state"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country,omitempty"`
}

// Trimmed returns a copy with surrounding whitespace removed from every field.
func (a ShipTo) Trimmed() ShipTo {
	return ShipTo{
		Name: strings.TrimSpace(a.Name), Line1: strings.TrimSpace(a.Line1), Line2: strings.TrimSpace(a.Line2),
		City: strings.TrimSpace(a.City), State: strings.TrimSpace(a.State),
		PostalCode: strings.TrimSpace(a.PostalCode), Country: strings.TrimSpace(a.Country),
	}
}

// IsEmpty reports whether no address was recorded.
func (a ShipTo) IsEmpty() bool { return a.Trimmed() == ShipTo{} }

// MissingField returns a human label for the first required field that is
// blank (name, street, city, state, postal code), or "" if complete.
func (a ShipTo) MissingField() string {
	a = a.Trimmed()
	switch {
	case a.Name == "":
		return "recipient name"
	case a.Line1 == "":
		return "street address"
	case a.City == "":
		return "city"
	case a.State == "":
		return "state"
	case a.PostalCode == "":
		return "postal code"
	}
	return ""
}

// Lines renders the address as display lines (recipient, street, optional
// second street line, "City, ST 12345", optional country).
func (a ShipTo) Lines() []string {
	a = a.Trimmed()
	var out []string
	for _, l := range []string{a.Name, a.Line1, a.Line2} {
		if l != "" {
			out = append(out, l)
		}
	}
	if cityLine := strings.TrimSpace(a.City + ", " + a.State + " " + a.PostalCode); cityLine != "," {
		out = append(out, strings.TrimSpace(strings.TrimPrefix(cityLine, ",")))
	}
	if a.Country != "" {
		out = append(out, a.Country)
	}
	return out
}

// OneLine is Lines joined with ", " for compact spots like a card.
func (a ShipTo) OneLine() string { return strings.Join(a.Lines(), ", ") }

// LastShipTo returns the ship-to address of customerID's most recent order
// that recorded one, for prefilling a repeat order's form. A customer with
// no addressed orders yet yields the zero ShipTo, not an error.
func (s *Store) LastShipTo(ctx context.Context, tenantID, customerID int64) (ShipTo, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
SELECT shipping_address FROM orders
WHERE tenant_id = $1 AND customer_id = $2 AND shipping_address <> '{}'::jsonb
ORDER BY created_at DESC, id DESC
LIMIT 1`, tenantID, customerID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ShipTo{}, nil
	}
	if err != nil {
		return ShipTo{}, fmt.Errorf("load last ship-to for customer %d: %w", customerID, err)
	}
	return parseShipTo(raw), nil
}

func (a ShipTo) marshal() ([]byte, error) {
	if a.IsEmpty() {
		return []byte("{}"), nil
	}
	return json.Marshal(a.Trimmed())
}

func parseShipTo(raw []byte) ShipTo {
	var a ShipTo
	_ = json.Unmarshal(raw, &a) // a malformed legacy value just reads as no address
	return a
}
