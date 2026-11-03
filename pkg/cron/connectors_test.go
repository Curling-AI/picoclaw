package cron

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func connectorIDs(count int) []string {
	names := make([]string, count)
	for i := range names {
		names[i] = fmt.Sprintf("connector-%d", i)
	}
	return names
}

func TestNormalizeConnectors(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		want    []string
		wantErr bool
	}{
		{name: "nil stays empty", input: nil, want: nil},
		{name: "empty clears", input: []string{}, want: nil},
		{name: "keeps received order", input: []string{"slack", "gmail"}, want: []string{"slack", "gmail"}},
		{
			name:  "trims, lowercases and removes duplicates",
			input: []string{" Gmail ", "gmail"},
			want:  []string{"gmail"},
		},
		{name: "accepts underscore and hyphen", input: []string{"a_b-c"}, want: []string{"a_b-c"}},
		{name: "rejects blank", input: []string{"  "}, wantErr: true},
		{name: "rejects invalid character", input: []string{"gmail!"}, wantErr: true},
		{name: "rejects leading hyphen", input: []string{"-gmail"}, wantErr: true},
		{
			name:  "accepts 64 characters",
			input: []string{strings.Repeat("a", 64)},
			want:  []string{strings.Repeat("a", 64)},
		},
		{name: "rejects 65 characters", input: []string{strings.Repeat("a", 65)}, wantErr: true},
		{name: "accepts 32 items", input: connectorIDs(MaxConnectors), want: connectorIDs(MaxConnectors)},
		{name: "rejects 33 items", input: connectorIDs(MaxConnectors + 1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeConnectors(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
