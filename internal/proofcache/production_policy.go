package proofcache

import "path/filepath"

// IsProductionRuntimeInvocationAllowed reports whether runtime ABI 1 may
// inspect prepared state for an invocation. It intentionally excludes legacy
// lanes whose exact proof is not currently profitable in the product runtime.
func IsProductionRuntimeInvocationAllowed(cwd string, argv []string) bool {
	inv := NormalizeInvocation(cwd, argv)
	if productionRuntimeDirectAllowed(inv.PolicyArgv) {
		return true
	}
	script, ok := ComposedShellArgvScript(inv.OriginalArgv)
	if !ok {
		return false
	}
	plan, ok := parseComposedShell(script)
	return ok && productionRuntimePlanAllowed(inv.PolicyCWD, plan, plan.root, false)
}

func productionRuntimeDirectAllowed(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	switch filepath.Base(argv[0]) {
	case "file", "whoami", "id":
		return false
	}
	return IsReplayAllowed(argv) && !isToolVersionProbe(argv) && !isCommandPathLookup(argv)
}

func productionRuntimePlanAllowed(cwd string, plan shellPlan, index int, hasInput bool) bool {
	if index < 0 || index >= len(plan.nodes) {
		return false
	}
	node := plan.nodes[index]
	switch node.kind {
	case shellNodeExec:
		if hasInput {
			_, ok := evalComposedShellFilter(node.argv, nil)
			return ok
		}
		inv := NormalizeInvocation(cwd, node.argv)
		return productionRuntimeDirectAllowed(inv.PolicyArgv)
	case shellNodePipe:
		return productionRuntimePlanAllowed(cwd, plan, node.left, hasInput) &&
			productionRuntimePlanAllowed(cwd, plan, node.right, true)
	case shellNodeAnd, shellNodeSeq:
		return productionRuntimePlanAllowed(cwd, plan, node.left, hasInput) &&
			productionRuntimePlanAllowed(cwd, plan, node.right, hasInput)
	case shellNodeRedirNull:
		return productionRuntimePlanAllowed(cwd, plan, node.left, hasInput)
	default:
		return false
	}
}
