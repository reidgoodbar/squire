package kernel

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	composedShellMaxTokens = 128
	composedShellMaxNodes  = 128
	composedShellMaxArgs   = 32
	composedShellMaxWord   = 512
)

type shellTokenKind int

const (
	shellTokenWord shellTokenKind = iota
	shellTokenPipe
	shellTokenAnd
	shellTokenSemi
	shellTokenLParen
	shellTokenRParen
	shellTokenRedirNull
)

type shellToken struct {
	kind shellTokenKind
	text string
	fd   int
}

type shellNodeKind int

const (
	shellNodeExec shellNodeKind = iota
	shellNodePipe
	shellNodeAnd
	shellNodeSeq
	shellNodeRedirNull
	shellNodeFor
)

type shellNode struct {
	kind   shellNodeKind
	left   int
	right  int
	fd     int
	repeat int
	argv   []string
}

type shellPlan struct {
	nodes []shellNode
	root  int
}

type shellParser struct {
	tokens []shellToken
	pos    int
	plan   shellPlan
}

type shellEvalResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	nativeMS int64
}

func (k *Kernel) ReplayComposedShell(ctx context.Context, sessionID, cwd, script string) (RunResult, bool) {
	if k == nil || strings.TrimSpace(script) == "" {
		return RunResult{}, false
	}
	plan, ok := parseComposedShell(script)
	if !ok {
		return RunResult{}, false
	}
	start := time.Now()
	res, ok := k.evalComposedShellNode(ctx, sessionID, cwd, plan, plan.root, nil)
	if !ok || len(res.stdout)+len(res.stderr) > maxFastPathOutputBytes {
		return RunResult{}, false
	}
	return RunResult{
		Stdout:   res.stdout,
		Stderr:   res.stderr,
		ExitCode: res.exitCode,
		Mode:     ModeReplay,
		Family:   FamilyShellUnknown,
		Observation: Observation{
			StdoutHash:   hashBytes(res.stdout),
			StderrHash:   hashBytes(res.stderr),
			StdoutSize:   len(res.stdout),
			StderrSize:   len(res.stderr),
			ExitCode:     res.exitCode,
			NativeWallMS: res.nativeMS,
			Timestamp:    time.Now(),
		},
		Proof: &ProofRecord{
			OperationKeyMatched:        true,
			InputFingerprintsMatched:   true,
			InvalidationEpochUnchanged: true,
			OperatorAllowlisted:        true,
			OutputAvailable:            true,
			OutputExact:                true,
			PolicyAllowedReplay:        true,
			NativeFallbackAvailable:    true,
			OperationKey:               "composed-shell-adapter",
			Reason:                     "all shell leaves replayed from exact hot snapshots; pure filters evaluated in memory",
		},
		Phases: PhaseTimings{OutputMaterializeMS: elapsedMS(start)},
	}, true
}

func parseComposedShell(script string) (shellPlan, bool) {
	tokens, ok := tokenizeComposedShell(script)
	if !ok || len(tokens) == 0 {
		return shellPlan{}, false
	}
	p := &shellParser{tokens: tokens, plan: shellPlan{root: -1}}
	root, ok := p.parseSequence()
	if !ok || p.pos != len(tokens) || root < 0 {
		return shellPlan{}, false
	}
	p.plan.root = root
	return p.plan, true
}

func tokenizeComposedShell(script string) ([]shellToken, bool) {
	var tokens []shellToken
	for i := 0; i < len(script); {
		for i < len(script) && (script[i] == ' ' || script[i] == '\t' || script[i] == '\n') {
			if script[i] == '\n' && composedShellTokenCanEnd(tokens) {
				tokens = append(tokens, shellToken{kind: shellTokenSemi})
				i++
				for i < len(script) && (script[i] == ' ' || script[i] == '\t' || script[i] == '\n') {
					i++
				}
				break
			}
			i++
		}
		if i >= len(script) {
			break
		}
		if len(tokens) >= composedShellMaxTokens {
			return nil, false
		}
		switch script[i] {
		case '|':
			if i+1 < len(script) && script[i+1] == '|' {
				return nil, false
			}
			tokens = append(tokens, shellToken{kind: shellTokenPipe})
			i++
			continue
		case '&':
			if i+1 >= len(script) || script[i+1] != '&' {
				return nil, false
			}
			tokens = append(tokens, shellToken{kind: shellTokenAnd})
			i += 2
			continue
		case ';':
			tokens = append(tokens, shellToken{kind: shellTokenSemi})
			i++
			continue
		case '(':
			tokens = append(tokens, shellToken{kind: shellTokenLParen})
			i++
			continue
		case ')':
			tokens = append(tokens, shellToken{kind: shellTokenRParen})
			i++
			continue
		case '>':
			next, ok := tokenizeComposedNullRedirect(script, i+1)
			if !ok {
				return nil, false
			}
			tokens = append(tokens, shellToken{kind: shellTokenRedirNull, fd: 1})
			i = next
			continue
		case '1', '2':
			if i+1 < len(script) && script[i+1] == '>' {
				fd := int(script[i] - '0')
				next, ok := tokenizeComposedNullRedirect(script, i+2)
				if !ok {
					return nil, false
				}
				tokens = append(tokens, shellToken{kind: shellTokenRedirNull, fd: fd})
				i = next
				continue
			}
		}
		word, next, ok := tokenizeComposedWord(script, i)
		if !ok || word == "" {
			return nil, false
		}
		tokens = append(tokens, shellToken{kind: shellTokenWord, text: word})
		i = next
	}
	return tokens, len(tokens) > 0
}

func tokenizeComposedNullRedirect(script string, i int) (int, bool) {
	for i < len(script) && (script[i] == ' ' || script[i] == '\t') {
		i++
	}
	const target = "/dev/null"
	if !strings.HasPrefix(script[i:], target) {
		return 0, false
	}
	i += len(target)
	if i < len(script) && !isComposedWordMeta(script[i]) {
		return 0, false
	}
	return i, true
}

func tokenizeComposedWord(script string, i int) (string, int, bool) {
	var b strings.Builder
	for i < len(script) && !isComposedWordMeta(script[i]) {
		if script[i] == '\'' || script[i] == '"' {
			quote := script[i]
			i++
			for i < len(script) && script[i] != quote {
				if !isComposedQuotedWordByteAllowed(script[i], quote) || (quote == '"' && (script[i] == '$' || script[i] == '`' || script[i] == '\\' || script[i] == '!')) {
					return "", 0, false
				}
				if b.Len()+1 >= composedShellMaxWord {
					return "", 0, false
				}
				b.WriteByte(script[i])
				i++
			}
			if i >= len(script) || script[i] != quote {
				return "", 0, false
			}
			i++
			continue
		}
		if !isComposedWordByteAllowed(script[i]) {
			return "", 0, false
		}
		if b.Len()+1 >= composedShellMaxWord {
			return "", 0, false
		}
		b.WriteByte(script[i])
		i++
	}
	return b.String(), i, true
}

func composedShellTokenCanEnd(tokens []shellToken) bool {
	if len(tokens) == 0 {
		return false
	}
	switch tokens[len(tokens)-1].kind {
	case shellTokenWord, shellTokenRParen, shellTokenRedirNull:
		return true
	default:
		return false
	}
}

func isComposedWordMeta(c byte) bool {
	switch c {
	case 0, ' ', '\t', '\n', '|', '&', ';', '(', ')', '<', '>':
		return true
	default:
		return false
	}
}

func isComposedWordByteAllowed(c byte) bool {
	if c <= 0x1f || c >= 0x7f {
		return false
	}
	switch c {
	case '\\', '$', '`', '!', '*', '?', '~', '<', '{', '}', '[', ']', '#', '\'', '"':
		return false
	default:
		return true
	}
}

func isComposedQuotedWordByteAllowed(c byte, quote byte) bool {
	if quote == '\'' && c == '\\' {
		return true
	}
	return isComposedWordByteAllowed(c)
}

func (p *shellParser) parseSequence() (int, bool) {
	node, ok := p.parseAnd()
	if !ok {
		return -1, false
	}
	for {
		if !p.accept(shellTokenSemi) {
			break
		}
		for p.accept(shellTokenSemi) {
		}
		if p.peekKind(shellTokenRParen) || p.peekWord("done") || p.pos >= len(p.tokens) {
			break
		}
		right, ok := p.parseAnd()
		if !ok {
			return -1, false
		}
		node, ok = p.addNode(shellNode{kind: shellNodeSeq, left: node, right: right})
		if !ok {
			return -1, false
		}
	}
	return node, true
}

func (p *shellParser) parseAnd() (int, bool) {
	node, ok := p.parsePipeline()
	if !ok {
		return -1, false
	}
	for p.accept(shellTokenAnd) {
		right, ok := p.parsePipeline()
		if !ok {
			return -1, false
		}
		node, ok = p.addNode(shellNode{kind: shellNodeAnd, left: node, right: right})
		if !ok {
			return -1, false
		}
	}
	return node, true
}

func (p *shellParser) parsePipeline() (int, bool) {
	node, ok := p.parsePrimary()
	if !ok {
		return -1, false
	}
	for p.accept(shellTokenPipe) {
		right, ok := p.parsePrimary()
		if !ok {
			return -1, false
		}
		node, ok = p.addNode(shellNode{kind: shellNodePipe, left: node, right: right})
		if !ok {
			return -1, false
		}
	}
	return node, true
}

func (p *shellParser) parsePrimary() (int, bool) {
	var node int
	var ok bool
	if p.accept(shellTokenLParen) {
		node, ok = p.parseSequence()
		if !ok || !p.accept(shellTokenRParen) {
			return -1, false
		}
	} else if p.peekWord("for") {
		node, ok = p.parseFor()
		if !ok {
			return -1, false
		}
	} else {
		tok, ok := p.peek()
		if !ok || tok.kind != shellTokenWord {
			return -1, false
		}
		var argv []string
		for {
			tok, ok := p.peek()
			if !ok || tok.kind != shellTokenWord {
				break
			}
			if len(argv) >= composedShellMaxArgs {
				return -1, false
			}
			argv = append(argv, tok.text)
			p.pos++
		}
		if len(argv) == 0 || strings.Contains(argv[0], "=") {
			return -1, false
		}
		node, ok = p.addNode(shellNode{kind: shellNodeExec, argv: argv})
		if !ok {
			return -1, false
		}
	}
	for {
		tok, ok := p.peek()
		if !ok || tok.kind != shellTokenRedirNull {
			break
		}
		p.pos++
		node, ok = p.addNode(shellNode{kind: shellNodeRedirNull, left: node, fd: tok.fd})
		if !ok {
			return -1, false
		}
	}
	return node, true
}

func (p *shellParser) parseFor() (int, bool) {
	if !p.acceptWord("for") {
		return -1, false
	}
	variable, ok := p.nextWord()
	if !ok || !isComposedShellIdentifier(variable) || !p.acceptWord("in") {
		return -1, false
	}
	repeat := 0
	for {
		tok, ok := p.peek()
		if !ok {
			return -1, false
		}
		if tok.kind == shellTokenSemi {
			break
		}
		if tok.kind != shellTokenWord || tok.text == "do" || tok.text == "done" || tok.text == "in" {
			return -1, false
		}
		repeat++
		if repeat > 64 {
			return -1, false
		}
		p.pos++
	}
	if repeat == 0 || !p.accept(shellTokenSemi) {
		return -1, false
	}
	for p.accept(shellTokenSemi) {
	}
	if !p.acceptWord("do") {
		return -1, false
	}
	for p.accept(shellTokenSemi) {
	}
	body, ok := p.parseSequence()
	if !ok {
		return -1, false
	}
	for p.accept(shellTokenSemi) {
	}
	if !p.acceptWord("done") {
		return -1, false
	}
	return p.addNode(shellNode{kind: shellNodeFor, left: body, repeat: repeat})
}

func isComposedShellIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 {
			if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && r != '_' {
				return false
			}
			continue
		}
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func (p *shellParser) addNode(node shellNode) (int, bool) {
	if len(p.plan.nodes) >= composedShellMaxNodes {
		return -1, false
	}
	p.plan.nodes = append(p.plan.nodes, node)
	return len(p.plan.nodes) - 1, true
}

func (p *shellParser) accept(kind shellTokenKind) bool {
	if p.peekKind(kind) {
		p.pos++
		return true
	}
	return false
}

func (p *shellParser) acceptWord(text string) bool {
	if p.peekWord(text) {
		p.pos++
		return true
	}
	return false
}

func (p *shellParser) nextWord() (string, bool) {
	tok, ok := p.peek()
	if !ok || tok.kind != shellTokenWord {
		return "", false
	}
	p.pos++
	return tok.text, true
}

func (p *shellParser) peekWord(text string) bool {
	tok, ok := p.peek()
	return ok && tok.kind == shellTokenWord && tok.text == text
}

func (p *shellParser) peekKind(kind shellTokenKind) bool {
	tok, ok := p.peek()
	return ok && tok.kind == kind
}

func (p *shellParser) peek() (shellToken, bool) {
	if p.pos >= len(p.tokens) {
		return shellToken{}, false
	}
	return p.tokens[p.pos], true
}

func (k *Kernel) evalComposedShellNode(ctx context.Context, sessionID, cwd string, plan shellPlan, idx int, input []byte) (shellEvalResult, bool) {
	if idx < 0 || idx >= len(plan.nodes) {
		return shellEvalResult{}, false
	}
	node := plan.nodes[idx]
	switch node.kind {
	case shellNodeExec:
		if input != nil {
			return evalComposedShellFilter(node.argv, input)
		}
		return k.evalComposedShellSource(ctx, sessionID, cwd, node.argv)
	case shellNodePipe:
		left, ok := k.evalComposedShellNode(ctx, sessionID, cwd, plan, node.left, input)
		if !ok {
			return shellEvalResult{}, false
		}
		right, ok := k.evalComposedShellNode(ctx, sessionID, cwd, plan, node.right, left.stdout)
		if !ok {
			return shellEvalResult{}, false
		}
		return shellEvalResult{
			stdout:   append([]byte(nil), right.stdout...),
			stderr:   append(append([]byte(nil), left.stderr...), right.stderr...),
			exitCode: right.exitCode,
			nativeMS: left.nativeMS + right.nativeMS,
		}, true
	case shellNodeAnd:
		left, ok := k.evalComposedShellNode(ctx, sessionID, cwd, plan, node.left, input)
		if !ok {
			return shellEvalResult{}, false
		}
		if left.exitCode != 0 {
			return left, true
		}
		right, ok := k.evalComposedShellNode(ctx, sessionID, cwd, plan, node.right, input)
		if !ok {
			return shellEvalResult{}, false
		}
		return concatComposedShellResults(left, right), true
	case shellNodeSeq:
		left, ok := k.evalComposedShellNode(ctx, sessionID, cwd, plan, node.left, input)
		if !ok {
			return shellEvalResult{}, false
		}
		right, ok := k.evalComposedShellNode(ctx, sessionID, cwd, plan, node.right, input)
		if !ok {
			return shellEvalResult{}, false
		}
		return concatComposedShellResults(left, right), true
	case shellNodeRedirNull:
		res, ok := k.evalComposedShellNode(ctx, sessionID, cwd, plan, node.left, input)
		if !ok {
			return shellEvalResult{}, false
		}
		switch node.fd {
		case 1:
			res.stdout = nil
		case 2:
			res.stderr = nil
		default:
			return shellEvalResult{}, false
		}
		return res, true
	case shellNodeFor:
		if input != nil || node.repeat <= 0 || node.repeat > 64 {
			return shellEvalResult{}, false
		}
		var total shellEvalResult
		for i := 0; i < node.repeat; i++ {
			res, ok := k.evalComposedShellNode(ctx, sessionID, cwd, plan, node.left, nil)
			if !ok {
				return shellEvalResult{}, false
			}
			total = concatComposedShellResults(total, res)
		}
		return total, true
	default:
		return shellEvalResult{}, false
	}
}

func (k *Kernel) evalComposedShellSource(ctx context.Context, sessionID, cwd string, argv []string) (shellEvalResult, bool) {
	if len(argv) == 0 {
		return shellEvalResult{}, false
	}
	if filepath.Base(argv[0]) == "printf" {
		return evalComposedShellPrintf(argv)
	}
	inv := NormalizeInvocation(cwd, argv)
	if !IsReplayAllowed(inv.PolicyArgv) {
		return shellEvalResult{}, false
	}
	res, ok := k.FastReplayInvocation(ctx, sessionID, inv)
	if !ok || res.Mode != ModeReplay {
		return shellEvalResult{}, false
	}
	return shellEvalResult{
		stdout:   append([]byte(nil), res.Stdout...),
		stderr:   append([]byte(nil), res.Stderr...),
		exitCode: res.ExitCode,
		nativeMS: res.Observation.NativeWallMS,
	}, true
}

func evalComposedShellPrintf(argv []string) (shellEvalResult, bool) {
	if len(argv) != 2 || strings.Contains(argv[1], "%") {
		return shellEvalResult{}, false
	}
	out, ok := renderComposedShellPrintfFormat(argv[1])
	if !ok {
		return shellEvalResult{}, false
	}
	return shellEvalResult{stdout: out, exitCode: 0}, true
}

func renderComposedShellPrintfFormat(format string) ([]byte, bool) {
	if len(format) > 4096 {
		return nil, false
	}
	out := make([]byte, 0, len(format))
	for i := 0; i < len(format); i++ {
		if format[i] != '\\' {
			if format[i] == 0 {
				return nil, false
			}
			out = append(out, format[i])
			continue
		}
		i++
		if i >= len(format) {
			return nil, false
		}
		switch format[i] {
		case '\\':
			out = append(out, '\\')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		default:
			return nil, false
		}
	}
	return out, true
}

func evalComposedShellFilter(argv []string, input []byte) (shellEvalResult, bool) {
	if len(argv) == 0 {
		return shellEvalResult{}, false
	}
	name := filepath.Base(argv[0])
	switch name {
	case "cat":
		if len(argv) != 1 {
			return shellEvalResult{}, false
		}
		return shellEvalResult{stdout: append([]byte(nil), input...), exitCode: 0}, true
	case "head":
		n, ok := parseComposedShellFilterLineCount(argv)
		if !ok {
			return shellEvalResult{}, false
		}
		return shellEvalResult{stdout: sedPrintRangeBytes(input, 1, n), exitCode: 0}, true
	case "tail":
		n, ok := parseComposedShellFilterLineCount(argv)
		if !ok {
			return shellEvalResult{}, false
		}
		return shellEvalResult{stdout: tailLineBytes(input, n), exitCode: 0}, true
	case "grep":
		pattern, quiet, ok := parseComposedShellFilterGrep(argv)
		if !ok || bytes.IndexByte(input, 0) >= 0 {
			return shellEvalResult{}, false
		}
		stdout, matched := fixedGrepOutput(input, []byte(pattern), quiet)
		if !matched {
			return shellEvalResult{exitCode: 1}, true
		}
		return shellEvalResult{stdout: stdout, exitCode: 0}, true
	case "wc":
		if len(argv) != 2 || argv[1] != "-l" {
			return shellEvalResult{}, false
		}
		return shellEvalResult{stdout: []byte(formatComposedShellWCLineCount(bytes.Count(input, []byte{'\n'}))), exitCode: 0}, true
	case "sort":
		if len(argv) != 1 || bytes.IndexByte(input, 0) >= 0 {
			return shellEvalResult{}, false
		}
		out, ok := sortComposedShellLines(input)
		if !ok {
			return shellEvalResult{}, false
		}
		return shellEvalResult{stdout: out, exitCode: 0}, true
	default:
		return shellEvalResult{}, false
	}
}

func formatComposedShellWCLineCount(n int) string {
	if runtime.GOOS == "darwin" {
		return fmt.Sprintf("%8d\n", n)
	}
	return fmt.Sprintf("%d\n", n)
}

func sortComposedShellLines(input []byte) ([]byte, bool) {
	if len(input) == 0 {
		return nil, true
	}
	lines := bytes.Split(input, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	sort.Slice(lines, func(i, j int) bool {
		return bytes.Compare(lines[i], lines[j]) < 0
	})
	var out []byte
	for _, line := range lines {
		out = append(out, line...)
		out = append(out, '\n')
		if len(out) > maxFastPathOutputBytes {
			return nil, false
		}
	}
	return out, true
}

func parseComposedShellFilterLineCount(argv []string) (int, bool) {
	if len(argv) == 3 && argv[1] == "-n" {
		return parseComposedShellNonNegativeLineCount(argv[2])
	}
	if len(argv) == 2 && strings.HasPrefix(argv[1], "-n") && len(argv[1]) > 2 {
		return parseComposedShellNonNegativeLineCount(argv[1][2:])
	}
	return 0, false
}

func parseComposedShellNonNegativeLineCount(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 100000 {
			return 0, false
		}
	}
	return n, true
}

func parseComposedShellFilterGrep(argv []string) (string, bool, bool) {
	var pattern string
	var quiet bool
	var fixed bool
	for _, arg := range argv[1:] {
		switch arg {
		case "-q":
			quiet = true
		case "-F":
			fixed = true
		default:
			if strings.HasPrefix(arg, "-") || pattern != "" {
				return "", false, false
			}
			pattern = arg
		}
	}
	if !fixed || pattern == "" || strings.ContainsAny(pattern, "\x00\n\r") {
		return "", false, false
	}
	return pattern, quiet, true
}

func concatComposedShellResults(left, right shellEvalResult) shellEvalResult {
	stdout := make([]byte, 0, len(left.stdout)+len(right.stdout))
	stdout = append(stdout, left.stdout...)
	stdout = append(stdout, right.stdout...)
	stderr := make([]byte, 0, len(left.stderr)+len(right.stderr))
	stderr = append(stderr, left.stderr...)
	stderr = append(stderr, right.stderr...)
	return shellEvalResult{
		stdout:   stdout,
		stderr:   stderr,
		exitCode: right.exitCode,
		nativeMS: left.nativeMS + right.nativeMS,
	}
}

func ComposedShellArgvScript(argv []string) (string, bool) {
	if len(argv) != 3 {
		return "", false
	}
	if argv[1] != "-c" && argv[1] != "-lc" {
		return "", false
	}
	switch filepath.Base(argv[0]) {
	case "sh", "bash", "zsh":
		return argv[2], true
	default:
		return "", false
	}
}

func ComposedShellScriptLooksParseable(script string) bool {
	_, ok := parseComposedShell(script)
	return ok
}
