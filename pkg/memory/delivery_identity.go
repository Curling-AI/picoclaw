package memory

import (
	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// Only explicit append entry points create occurrences; rewrites and retries
// keep the already prepared ID independently of history ordering evidence.
func newDeliveryOccurrence(message providers.Message) providers.Message {
	if message.Delivery != nil {
		delivery := *message.Delivery
		delivery.ID = uuid.NewString()
		message.Delivery = &delivery
	}
	return message
}

func validDeliveryID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed != uuid.Nil && parsed.String() == id
}

func checkedDeliveryIdentity(message providers.Message, counts map[string]int) providers.Message {
	if message.Delivery != nil && (!validDeliveryID(message.Delivery.ID) || counts[message.Delivery.ID] != 1) {
		delivery := *message.Delivery
		delivery.ID = ""
		message.Delivery = &delivery
	}
	return message
}

func contentDeliveryIDCounts(rows []contentRawRow) map[string]int {
	counts := make(map[string]int)
	for _, row := range rows {
		if row.Message.Delivery != nil {
			counts[row.Message.Delivery.ID]++
		}
	}
	return counts
}
