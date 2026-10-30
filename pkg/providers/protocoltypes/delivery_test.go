package protocoltypes

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDeliveryDecodePreservesAbsentFieldsAndClearsInvalidIdentity(t *testing.T) {
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	initial := Delivery{ID: "550e8400-e29b-41d4-a716-446655440000", Origin: "cron:job", At: at, Self: true}
	for _, data := range []string{`{}`, `null`, `{"id":42}`, `{"id":null}`} {
		delivery := initial
		if err := json.Unmarshal([]byte(data), &delivery); err != nil {
			t.Fatal(err)
		}
		want := initial
		if data != `{}` && data != `null` {
			want.ID = ""
		}
		if delivery != want {
			t.Fatal("optional identity changed ordinary JSON field semantics", data, delivery, want)
		}
	}
}
