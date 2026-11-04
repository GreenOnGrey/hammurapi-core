package specdata

import "testing"

func TestToSummarySkippedAreas(t *testing.T) {
	if got := ToSummary(ListedFeature{}).SkippedAreas; got == nil || len(got) != 0 {
		t.Fatalf("no skipped areas: want an empty list, got %#v", got)
	}
	f := ListedFeature{Feature: Feature{SkippedAreas: []string{"design"}}}
	if got := ToSummary(f).SkippedAreas; len(got) != 1 || got[0] != "design" {
		t.Fatalf("skipped areas: got %#v", got)
	}
}
