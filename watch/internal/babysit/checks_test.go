package babysit

import "testing"

// TestCountBucketsTally pins countBuckets' contract directly (#1129): a
// package-level tally helper both automergeCIHold and ciStatus draw their
// counts from, returning named counters (not a map[string]int, per
// watch/docs/go-gotchas.md's closed-set-enum discipline) for every bucket gh
// 2.97 reports plus the empty-string and unrecognized-value degenerate
// cases, and a running Total.
func TestCountBucketsTally(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []check
		want   bucketTally
	}{
		{"empty slice", nil, bucketTally{}},
		{"single pass", []check{{Bucket: "pass"}}, bucketTally{Pass: 1, Total: 1}},
		{"single fail", []check{{Bucket: "fail"}}, bucketTally{Fail: 1, Total: 1}},
		{"single pending", []check{{Bucket: "pending"}}, bucketTally{Pending: 1, Total: 1}},
		{"single cancel", []check{{Bucket: "cancel"}}, bucketTally{Cancel: 1, Total: 1}},
		{"single skipping", []check{{Bucket: "skipping"}}, bucketTally{Skipping: 1, Total: 1}},
		{"single empty bucket string", []check{{Bucket: ""}}, bucketTally{Empty: 1, Total: 1}},
		{"single unrecognized bucket", []check{{Bucket: "neutral"}}, bucketTally{Unknown: 1, Total: 1}},
		{
			"one of every bucket",
			[]check{
				{Bucket: "pass"}, {Bucket: "fail"}, {Bucket: "pending"},
				{Bucket: "cancel"}, {Bucket: "skipping"}, {Bucket: ""}, {Bucket: "neutral"},
			},
			bucketTally{Pass: 1, Fail: 1, Pending: 1, Cancel: 1, Skipping: 1, Empty: 1, Unknown: 1, Total: 7},
		},
		{
			"repeated buckets accumulate",
			[]check{{Bucket: "pass"}, {Bucket: "pass"}, {Bucket: "skipping"}, {Bucket: "skipping"}, {Bucket: "skipping"}},
			bucketTally{Pass: 2, Skipping: 3, Total: 5},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := countBuckets(tc.checks)
			if got != tc.want {
				t.Fatalf("countBuckets(%v) = %+v, want %+v", tc.checks, got, tc.want)
			}
		})
	}
}
