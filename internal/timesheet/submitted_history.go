package timesheet

import "sort"

// Selection sources (issue #235): where a staged interval's classification
// came from. They are recorded on each sidecar selection so status can show
// them before approval. An empty source is a selection written before the
// field existed.
const (
	SelectionSourceJudgment = "judgment"
	SelectionSourceHistory  = "history"
	SelectionSourceDefault  = "default"
)

// SubmittedSelection is one interval whose submission the provider confirmed,
// with the classification it was submitted under. Only confirmed write
// intents count: Gradion is the authority, so a saved-but-unsubmitted day is
// never history.
type SubmittedSelection struct {
	Date             string
	IntervalID       string
	ClassificationID string
	Name             string
	Code             string
}

// SubmittedSelections returns the newest confirmed submission per interval,
// excluding the given date, in date/interval order.
func (s *Session) SubmittedSelections(exclude string) []SubmittedSelection {
	return submittedSelections(s.store.State(), exclude)
}

func submittedSelections(state *State, exclude string) []SubmittedSelection {
	type newest struct {
		at  string
		sel SubmittedSelection
	}
	latest := map[[2]string]newest{}
	for _, in := range state.Intents {
		if in.Status != IntentConfirmed || in.Date == exclude || in.LocalID == "" {
			continue
		}
		// The verified remote content is the authority; the planned
		// operation is the fallback when the read did not expose it.
		classID := in.Operation.ClassificationID
		if in.Observed != nil && in.Observed.ClassificationID != "" {
			classID = in.Observed.ClassificationID
		}
		if classID == "" {
			continue
		}
		sel := SubmittedSelection{Date: in.Date, IntervalID: in.LocalID, ClassificationID: classID}
		if c := in.Operation.SelectedChoice; c != nil {
			sel.Name, sel.Code = c.Name, c.Code
		}
		key := [2]string{in.Date, in.LocalID}
		if prev, ok := latest[key]; ok && prev.at > in.RecordedAt {
			continue
		}
		latest[key] = newest{at: in.RecordedAt, sel: sel}
	}
	out := make([]SubmittedSelection, 0, len(latest))
	for _, n := range latest {
		out = append(out, n.sel)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].IntervalID < out[j].IntervalID
	})
	return out
}
