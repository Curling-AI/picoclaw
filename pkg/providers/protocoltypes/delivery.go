package protocoltypes

import "encoding/json"

// UnmarshalJSON keeps a malformed optional ID from discarding the delivery.
func (d *Delivery) UnmarshalJSON(data []byte) error {
	type delivery Delivery
	wire := struct {
		delivery
		ID json.RawMessage `json:"id"`
	}{delivery: delivery(*d)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*d = Delivery(wire.delivery)
	if len(wire.ID) > 0 {
		d.ID = ""
		var id string
		if json.Unmarshal(wire.ID, &id) == nil {
			d.ID = id
		}
	}
	return nil
}
