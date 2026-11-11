package timesheet

import (
	"fmt"
	"slices"
	"strings"
)

func SavedClassificationDigest(date string, revision int, confirmed []Interval) string {
	intervals := slices.Clone(confirmed)
	slices.SortFunc(intervals, func(a, b Interval) int { return strings.Compare(a.ID, b.ID) })
	var b strings.Builder
	fmt.Fprintf(&b, "saved-classification-v1 %q %d\n", date, revision)
	for _, iv := range intervals {
		fmt.Fprintf(&b, "%q\n", []string{iv.ID, iv.Date, iv.Start, iv.End, iv.Project,
			iv.Classification, iv.Description, iv.RemoteCatalogID, iv.RemoteClassificationID,
			iv.MigratedClassificationID})
		fmt.Fprintf(&b, "%q\n", iv.EvidenceRefs)
	}
	return sha256Hex([]byte(b.String()))
}

// SavedClassificationDigest reads only the durable day while the session owns the store lock.
func (s *Session) SavedClassificationDigest(date string) (string, error) {
	day := s.store.State().Days[date]
	if day == nil || len(day.Confirmed) == 0 {
		return "", newError(CodeStateInvalid, "no saved intervals for "+date)
	}
	return SavedClassificationDigest(date, day.Revision, day.Confirmed), nil
}
