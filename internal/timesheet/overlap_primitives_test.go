package timesheet

import (
	"reflect"
	"testing"
)

func TestMergeSpansOrdersClusterByStartEndThenID(t *testing.T) {
	spans := []powSpan{{id: "b", startM: 0, endM: 10}, {id: "a", startM: 0, endM: 10}, {id: "c", startM: 0, endM: 5}}
	if got, want := mergeSpans(spans), [][]int{{2, 1, 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeSpans = %v, want %v", got, want)
	}
}

func TestResolveBlockIDsAssignsSuffixesInAscendingIDOrder(t *testing.T) {
	got := resolveBlockIDs([]string{"a-2", "a", "a"}, []bool{false, false, false})
	if want := []string{"a-2-2", "a", "a-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveBlockIDs = %v, want %v", got, want)
	}
}

func TestJoinTextSkipsEmptyAndContainedTexts(t *testing.T) {
	if got, want := joinText("x", " x ", "", "y"), "x + y"; got != want {
		t.Fatalf("joinText = %q, want %q", got, want)
	}
}

func TestMinuteSpansAreHalfOpenAndTouchingUnionJoins(t *testing.T) {
	if (minuteSpan{0, 10}).contains(10) {
		t.Fatal("minuteSpan{0,10} contains its exclusive end")
	}
	if got, want := unionSpans([]minuteSpan{{10, 20}, {0, 10}}), []minuteSpan{{0, 20}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unionSpans = %v, want %v", got, want)
	}
}
