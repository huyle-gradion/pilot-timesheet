package timesheet

import "time"

const (
	stopWindowPassed = "window_passed"
	// commentWindowOverlap pulls the comment query's strict-after `since`
	// bound back far enough to cover a comment updated exactly at the day's
	// start. The client filter keeps the window itself exact.
	commentWindowOverlap = time.Minute
)

type commentWindow struct {
	windowEnd    time.Time
	coveredFrom  time.Time
	coveredUntil time.Time
}

func commentWindowSince(windowStart time.Time) string {
	return windowStart.UTC().Add(-commentWindowOverlap).Format(time.RFC3339)
}

func (w *commentWindow) observePage(docs []commentDoc) bool {
	var last time.Time
	for i := range docs {
		at, err := time.Parse(time.RFC3339, docs[i].CreatedAt)
		if err != nil {
			return false
		}
		if w.coveredFrom.IsZero() {
			w.coveredFrom = at
		}
		w.coveredUntil = at
		last = at
	}
	return len(docs) > 0 && !last.Before(w.windowEnd)
}
