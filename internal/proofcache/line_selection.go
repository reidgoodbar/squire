package proofcache

import "strings"

const (
	maxLineSelectionRanges     = 8
	maxLineSelectionLine       = 10000
	maxLineSelectionRangeWidth = 500
)

type lineRange struct {
	start int
	end   int
}

// lineSelection preserves sed program order. Evaluation remains line-major,
// matching sed's behavior when ranges overlap or repeat.
type lineSelection struct {
	ranges []lineRange
}

func parseSedPrintSelection(expr string) (lineSelection, bool) {
	clauses := strings.Split(expr, ";")
	if len(clauses) == 0 || len(clauses) > maxLineSelectionRanges {
		return lineSelection{}, false
	}
	selection := lineSelection{ranges: make([]lineRange, 0, len(clauses))}
	for _, clause := range clauses {
		if clause == "" || !strings.HasSuffix(clause, "p") {
			return lineSelection{}, false
		}
		body := strings.TrimSuffix(clause, "p")
		parts := strings.Split(body, ",")
		if len(parts) == 0 || len(parts) > 2 {
			return lineSelection{}, false
		}
		start, ok := parsePositiveSmallLine(parts[0])
		if !ok || start > maxLineSelectionLine {
			return lineSelection{}, false
		}
		end := start
		if len(parts) == 2 {
			end, ok = parsePositiveSmallLine(parts[1])
			if !ok || end > maxLineSelectionLine {
				return lineSelection{}, false
			}
		}
		if end < start || end-start > maxLineSelectionRangeWidth {
			return lineSelection{}, false
		}
		selection.ranges = append(selection.ranges, lineRange{start: start, end: end})
	}
	return selection, true
}

func singleLineSelection(start, end int) lineSelection {
	return lineSelection{ranges: []lineRange{{start: start, end: end}}}
}

func (s lineSelection) minStart() int {
	min := 0
	for _, r := range s.ranges {
		if min == 0 || r.start < min {
			min = r.start
		}
	}
	return min
}

func (s lineSelection) valid() bool {
	if len(s.ranges) == 0 || len(s.ranges) > maxLineSelectionRanges {
		return false
	}
	for _, r := range s.ranges {
		if r.start <= 0 || r.end < r.start {
			return false
		}
	}
	return true
}

func (s lineSelection) maxEnd() int {
	max := 0
	for _, r := range s.ranges {
		if r.end > max {
			max = r.end
		}
	}
	return max
}

func (s lineSelection) matchCount(line int) int {
	matches := 0
	for _, r := range s.ranges {
		if line >= r.start && line <= r.end {
			matches++
		}
	}
	return matches
}
