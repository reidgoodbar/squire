package proofcache

type warmFileOperationKind uint8

const (
	warmFileOperationNone warmFileOperationKind = iota
	warmFileOperationCat
	warmFileOperationSed
	warmFileOperationHead
	warmFileOperationTail
	warmFileOperationNL
	warmFileOperationGrep
	warmFileOperationRg
)

// warmFileOperation is a bounded execution plan applied to proven file bytes.
// File validity is independent of the plan, so adding read operators does not
// require new cache keys or proof rules.
type warmFileOperation struct {
	kind       warmFileOperationKind
	path       string
	selection  lineSelection
	lineCount  int
	pattern    string
	quiet      bool
	lineNumber bool
}

func parseWarmFileOperation(argv []string) (warmFileOperation, bool) {
	argv = normalizeArgvForPolicy(argv)
	if isReplayableCatFileRead(argv) {
		return warmFileOperation{kind: warmFileOperationCat, path: argv[1]}, true
	}
	if isBoundedSedPrint(argv) {
		selection, ok := parseSedPrintSelection(argv[2])
		if !ok {
			return warmFileOperation{}, false
		}
		return warmFileOperation{kind: warmFileOperationSed, path: argv[3], selection: selection}, true
	}
	if isBoundedHeadPrint(argv) {
		path, count, ok := parseHeadTailArgs(argv, false)
		if !ok {
			return warmFileOperation{}, false
		}
		return warmFileOperation{kind: warmFileOperationHead, path: path, lineCount: count}, true
	}
	if isBoundedTailPrint(argv) {
		path, count, ok := parseHeadTailArgs(argv, true)
		if !ok {
			return warmFileOperation{}, false
		}
		return warmFileOperation{kind: warmFileOperationTail, path: path, lineCount: count}, true
	}
	if isNumberedAllLines(argv) {
		return warmFileOperation{kind: warmFileOperationNL, path: argv[2]}, true
	}
	if isFixedGrepFileSearch(argv) {
		pattern, path, quiet, ok := parseFixedGrepArgs(argv)
		if !ok {
			return warmFileOperation{}, false
		}
		return warmFileOperation{kind: warmFileOperationGrep, path: path, pattern: pattern, quiet: quiet}, true
	}
	if isFixedRgFileSearch(argv) {
		pattern, path, quiet, lineNumber, ok := parseFixedRgArgs(argv)
		if !ok {
			return warmFileOperation{}, false
		}
		return warmFileOperation{
			kind:       warmFileOperationRg,
			path:       path,
			pattern:    pattern,
			quiet:      quiet,
			lineNumber: lineNumber,
		}, true
	}
	return warmFileOperation{}, false
}
