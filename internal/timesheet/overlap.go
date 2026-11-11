package timesheet

import (
	"sort"
	"strconv"
	"strings"
)

type powSpan struct {
	id           string
	kind         string
	startM, endM int
}

func mergeSpans(spans []powSpan) [][]int {
	order := make([]int, len(spans))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		ia, ib := spans[order[a]], spans[order[b]]
		if ia.startM != ib.startM {
			return ia.startM < ib.startM
		}
		if ia.endM != ib.endM {
			return ia.endM < ib.endM
		}
		return ia.id < ib.id
	})
	var clusters [][]int
	var ends []int
	for _, i := range order {
		last := len(clusters) - 1
		if last >= 0 && spans[i].startM < ends[last] {
			clusters[last] = append(clusters[last], i)
			if spans[i].endM > ends[last] {
				ends[last] = spans[i].endM
			}
			continue
		}
		clusters = append(clusters, []int{i})
		ends = append(ends, spans[i].endM)
	}
	return clusters
}

func resolveBlockIDs(ids []string, fixed []bool) []string {
	out := append([]string(nil), ids...)
	used := make(map[string]bool, len(ids))
	for i := range ids {
		if fixed[i] && !used[ids[i]] {
			used[ids[i]] = true
			continue
		}
		fixed[i] = false
	}
	order := make([]int, 0, len(ids))
	for i := range ids {
		if !fixed[i] {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return ids[order[a]] < ids[order[b]] })
	for _, i := range order {
		id := ids[i]
		if !used[id] {
			used[id] = true
			out[i] = id
			continue
		}
		for n := 2; ; n++ {
			candidate := id + "-" + strconv.Itoa(n)
			if !used[candidate] {
				used[candidate] = true
				out[i] = candidate
				break
			}
		}
	}
	return out
}

func joinText(texts ...string) string {
	out := ""
	for _, t := range texts {
		t = strings.TrimSpace(t)
		if t == "" || strings.Contains(out, t) {
			continue
		}
		if out == "" {
			out = t
			continue
		}
		out += " + " + t
	}
	return out
}

// minuteSpan is a half-open [start, end) interval on the recorded day's local
// wall clock, in minutes since local midnight.
type minuteSpan struct{ start, end int }

func (s minuteSpan) contains(m int) bool { return s.start <= m && m < s.end }

func containsMinute(spans []minuteSpan, m int) bool {
	for _, s := range spans {
		if s.contains(m) {
			return true
		}
	}
	return false
}

func unionSpans(in []minuteSpan) []minuteSpan {
	if len(in) == 0 {
		return nil
	}
	sorted := append([]minuteSpan(nil), in...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	out := []minuteSpan{sorted[0]}
	for _, s := range sorted[1:] {
		if last := &out[len(out)-1]; s.start <= last.end {
			last.end = max(last.end, s.end)
		} else {
			out = append(out, s)
		}
	}
	return out
}

// cut must be sorted and disjoint, which every caller guarantees: spans of the
// occupied mask come from unionSpans, and the lunch rule passes one span.
func subtractSpans(base, cut []minuteSpan) []minuteSpan {
	var out []minuteSpan
	for _, b := range base {
		cursor := b.start
		for _, c := range cut {
			if c.start >= b.end {
				break
			}
			if c.end <= cursor {
				continue
			}
			if c.start > cursor {
				out = append(out, minuteSpan{cursor, c.start})
			}
			cursor = max(cursor, c.end)
		}
		if cursor < b.end {
			out = append(out, minuteSpan{cursor, b.end})
		}
	}
	return out
}
