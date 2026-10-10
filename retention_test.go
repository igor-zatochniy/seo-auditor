package main

import "testing"

func TestRetentionRequiresBoundedExplicitOptions(t *testing.T) {
	for _, opts := range []retentionOptions{
		{KeepDays: 0, MaxRuns: 100}, {KeepDays: 6, MaxRuns: 100},
		{KeepDays: 90, MaxRuns: 0}, {KeepDays: 90, MaxRuns: 1001},
		{KeepDays: 90, MaxRuns: 1, ForgetSchedule: "not-a-uuid"},
	} {
		if err := validateRetentionOptions(opts); err == nil {
			t.Fatalf("unbounded options accepted: %+v", opts)
		}
	}
	if err := validateRetentionOptions(retentionOptions{KeepDays: 90, MaxRuns: 100}); err != nil {
		t.Fatal(err)
	}
}
