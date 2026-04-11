package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/robfig/cron/v3"
)

const schedulesFile = "schedules.json"

// ScheduledPingRecord is the on-disk representation of a scheduled ping.
// Once: true means it's a one-shot ping that removes itself after firing.
type ScheduledPingRecord struct {
	ID      int    `json:"id"`
	Cron    string `json:"cron"`
	Target  string `json:"target"`
	Message string `json:"message"`
	Once    bool   `json:"once,omitempty"`
}

// ScheduledPing is the runtime version — adds CronID which is the scheduler's
// internal handle for the job. Not saved to disk since it changes every run.
type ScheduledPing struct {
	ScheduledPingRecord
	CronID cron.EntryID
}

// saveSchedules writes the current scheduledPings slice to schedules.json.
// Called any time a ping is added or removed.
func saveSchedules() error {
	records := make([]ScheduledPingRecord, len(scheduledPings))
	for i, p := range scheduledPings {
		records[i] = p.ScheduledPingRecord
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(schedulesFile, data, 0644)
}

// loadSchedules reads schedules.json on startup and re-registers all saved
// pings with the cron scheduler. Invalid cron expressions are skipped with
// a warning rather than crashing. One-shot pings (Once: true) are wrapped
// so they remove and save themselves after firing.
func loadSchedules() {
	data, err := os.ReadFile(schedulesFile)
	if os.IsNotExist(err) {
		fmt.Println("No schedules file found, starting fresh.")
		return
	}
	if err != nil {
		fmt.Println("Error reading schedules file:", err)
		return
	}

	var records []ScheduledPingRecord
	if err := json.Unmarshal(data, &records); err != nil {
		fmt.Println("Error parsing schedules file:", err)
		return
	}

	loaded := 0
	for _, r := range records {
		var cronID cron.EntryID
		var err error

		if r.Once {
			// One-shot: wrap the send in a self-removing closure
			record := r // capture loop var
			cronID, err = scheduler.AddFunc(record.Cron, func() {
				sendByTarget(record.Target, record.Message)
				removeScheduledPing(record.ID)
				if err := saveSchedules(); err != nil {
					fmt.Println("Warning: failed to save schedules after one-shot fired:", err)
				}
			})
		} else {
			// Recurring: just send
			cronID, err = registerCronJob(r.Target, r.Cron, r.Message)
		}

		if err != nil {
			fmt.Printf("Warning: skipping schedule #%d (%q) — invalid cron expression: %v\n", r.ID, r.Cron, err)
			continue
		}

		scheduledPings = append(scheduledPings, ScheduledPing{
			ScheduledPingRecord: r,
			CronID:              cronID,
		})
		if r.ID >= nextPingID {
			nextPingID = r.ID + 1
		}
		loaded++
	}
	fmt.Printf("Loaded %d/%d scheduled ping(s) from %s\n", loaded, len(records), schedulesFile)
}
