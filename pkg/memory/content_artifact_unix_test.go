//go:build unix

package memory

import (
	"encoding/json"
	"testing"
)

func TestContentDeviceIdentityPreservesPersistedBitsAcrossUnixTypes(t *testing.T) {
	for _, test := range []struct {
		name   string
		volume uint64
		wire   string
	}{
		{
			name:   "signed32-negative",
			volume: contentDeviceIdentity(int32(-1)),
			wire:   `{"volume":18446744073709551615,"file":7}`,
		},
		{
			name:   "signed32-high-bit",
			volume: contentDeviceIdentity(int32(-2147483648)),
			wire:   `{"volume":18446744071562067968,"file":7}`,
		},
		{
			name:   "unsigned32-high-bits",
			volume: contentDeviceIdentity(uint32(4294967295)),
			wire:   `{"volume":4294967295,"file":7}`,
		},
		{
			name:   "unsigned64-high-bit",
			volume: contentDeviceIdentity(uint64(9223372036854775808)),
			wire:   `{"volume":9223372036854775808,"file":7}`,
		},
		{
			name:   "unsigned64-all-bits",
			volume: contentDeviceIdentity(uint64(18446744073709551615)),
			wire:   `{"volume":18446744073709551615,"file":7}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := contentFileIdentity{Volume: test.volume, File: 7}
			data, err := json.Marshal(identity)
			if err != nil || string(data) != test.wire {
				t.Fatal("persisted device identity changed", string(data), err)
			}
			var restored contentFileIdentity
			if err = json.Unmarshal(data, &restored); err != nil || restored != identity {
				t.Fatal("device identity did not survive its persisted representation", restored, err)
			}
		})
	}
}
