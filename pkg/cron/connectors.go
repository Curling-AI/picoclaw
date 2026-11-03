package cron

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// MaxConnectors is above the 6 an agent declares at once: a person may tick more in the editor.
const MaxConnectors = 32

var connectorName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// NormalizeConnectors checks only the format: a custom or removed connector stays a valid saved value.
func NormalizeConnectors(names []string) ([]string, error) {
	if len(names) > MaxConnectors {
		return nil, fmt.Errorf("connectors must list at most %d items", MaxConnectors)
	}
	var normalized []string
	for _, name := range names {
		id := strings.ToLower(strings.TrimSpace(name))
		if !connectorName.MatchString(id) {
			return nil, fmt.Errorf("invalid connector ID %q", name)
		}
		if !slices.Contains(normalized, id) {
			normalized = append(normalized, id)
		}
	}
	return normalized, nil
}
