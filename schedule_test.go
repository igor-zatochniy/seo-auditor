package main

import (
	"testing"
	"time"
)

func TestScheduleSkipsMissedIntervals(t *testing.T) {
	due := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		now   time.Time
		hours int
		want  time.Time
	}{
		{due.Add(-time.Minute), 24, due},
		{due, 24, due.Add(24 * time.Hour)},
		{due.Add(73 * time.Hour), 24, due.Add(96 * time.Hour)},
		{due.Add(48 * time.Hour), 24, due.Add(72 * time.Hour)},
	} {
		if got := nextScheduleTime(due, tc.now, tc.hours); !got.Equal(tc.want) {
			t.Fatalf("got %v want %v", got, tc.want)
		}
	}
}

func TestScheduleRejectsUnsafeAndAmbiguousInput(t *testing.T) {
	valid := scheduleInput{ID: newWebRunID(), Name: "Щоденний аудит", URLs: "https://example.com/a?token=private", IntervalHours: 24, NextRunAt: time.Now().Add(time.Hour)}
	source, label, count, err := prepareSchedule(&valid, Config{}, 100)
	if err != nil || count != 1 || len(source.URLs) != 1 || label == source.URLs[0] {
		t.Fatalf("source=%v label=%q count=%d err=%v", source, label, count, err)
	}
	for _, change := range []func(*scheduleInput){
		func(i *scheduleInput) { i.URLs = "http://127.0.0.1/private" },
		func(i *scheduleInput) { i.IntervalHours = 0 },
		func(i *scheduleInput) { i.IntervalHours = 721 },
		func(i *scheduleInput) { i.NextRunAt = time.Now().Add(-time.Hour) },
		func(i *scheduleInput) {
			i.Mode = "site"
			i.Site = &siteCrawlOptions{RootURL: "https://example.com/", MaxPages: 10}
		},
		func(i *scheduleInput) { i.Name = "" },
	} {
		bad := valid
		change(&bad)
		if _, _, _, err = prepareSchedule(&bad, Config{}, 100); err == nil {
			t.Fatalf("invalid input accepted: %+v", bad)
		}
	}
}
