package cron

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStartRepairsPersistedLegacyNamesWithoutChangingJobs(t *testing.T) {
	every := int64(3600000)
	lastRun := int64(1000)
	original := CronStore{Version: 1, Jobs: []CronJob{
		{
			ID:       "legacy",
			Name:     "Rodada da automação: Resu...",
			Enabled:  false,
			Schedule: CronSchedule{Kind: "every", EveryMS: &every},
			Payload: CronPayload{
				Kind:      "agent_turn",
				Message:   "Rodada da automação: Resumo diário de notícias",
				Channel:   "grpc",
				To:        "original-run",
				LoopSlug:  "my-loop",
				RoundKind: "automation",
			},
			State: CronJobState{
				LastRunAtMS: &lastRun,
				LastStatus:  "ok",
			},
			CreatedAtMS:  500,
			UpdatedAtMS:  1000,
			SkipWhenIdle: true,
		},
		{
			ID:      "untouched",
			Name:    "Meu radar de notícias",
			Enabled: false,
			Payload: CronPayload{Message: "Pesquisar notícias"},
		},
		{ID: "topic", Name: "Como funciona a Rodada da automação", Enabled: false},
	}}
	path := filepath.Join(t.TempDir(), "jobs.json")
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	service := NewCronService(path, nil)
	if startErr := service.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	service.Stop()
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved CronStore
	if decodeErr := json.Unmarshal(first, &saved); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	want := original
	want.Jobs[0].Name = "Resumo diário de notícias"
	if !reflect.DeepEqual(saved, want) {
		t.Fatalf("name repair changed other fields: %+v", saved)
	}
	if startErr := service.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	service.Stop()
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("second startup changed the repaired store")
	}
}
