package protocol

func TrustTierRank(tier string) int {
	switch tier {
	case TrustAdmin:
		return 4
	case TrustScaleAllowed:
		return 3
	case TrustTrusted:
		return 2
	case TrustAnonymous, TrustGitHubBasic:
		return 1
	default:
		return 0
	}
}

func TrustTierAtLeast(actual, required string) bool {
	return TrustTierRank(actual) >= TrustTierRank(required)
}

func HasScope(scopes []string, required string) bool {
	allowed := scopeAliases(required)
	for _, scope := range scopes {
		for _, candidate := range allowed {
			if scope == candidate || scope == ScopeAdmin {
				return true
			}
		}
	}
	return false
}

func AllowedTokenScopesForTier(tier string) []string {
	switch tier {
	case TrustAnonymous:
		return []string{
			ScopeUserRead,
			ScopeVerifyRun,
			ScopeDataRun,
			ScopeMediaRun,
			ScopeDepsRun,
			ScopeSQLRun,
			ScopeCompileRun,
			ScopeSolveRun,
			ScopeQuantumRun,
			ScopeTestRun,
			ScopeLintRun,
			ScopeAuditRun,
			ScopeBuildRun,
			ScopeBenchRun,
			ScopeBrowserRun,
			ScopeScaleData,
			ScopeScaleMedia,
		}
	case TrustAdmin:
		return []string{
			ScopeUserRead,
			ScopeVerifyRun,
			ScopeDataRun,
			ScopeMediaRun,
			ScopeDepsRun,
			ScopeSQLRun,
			ScopeCompileRun,
			ScopeSolveRun,
			ScopeQuantumRun,
			ScopeTestRun,
			ScopeLintRun,
			ScopeAuditRun,
			ScopeBuildRun,
			ScopeBenchRun,
			ScopeBrowserRun,
			ScopeScaleData,
			ScopeScaleMedia,
			ScopeAdmin,
		}
	case TrustScaleAllowed:
		return []string{
			ScopeUserRead,
			ScopeVerifyRun,
			ScopeDataRun,
			ScopeMediaRun,
			ScopeDepsRun,
			ScopeSQLRun,
			ScopeCompileRun,
			ScopeSolveRun,
			ScopeQuantumRun,
			ScopeTestRun,
			ScopeLintRun,
			ScopeAuditRun,
			ScopeBuildRun,
			ScopeBenchRun,
			ScopeBrowserRun,
			ScopeScaleData,
			ScopeScaleMedia,
		}
	case TrustTrusted:
		return []string{
			ScopeUserRead,
			ScopeVerifyRun,
			ScopeDataRun,
			ScopeMediaRun,
			ScopeDepsRun,
			ScopeSQLRun,
			ScopeCompileRun,
			ScopeSolveRun,
			ScopeQuantumRun,
			ScopeTestRun,
			ScopeLintRun,
			ScopeAuditRun,
			ScopeBuildRun,
			ScopeBenchRun,
			ScopeBrowserRun,
			ScopeScaleData,
			ScopeScaleMedia,
		}
	case TrustGitHubBasic:
		return []string{
			ScopeUserRead,
			ScopeVerifyRun,
			ScopeDataRun,
			ScopeMediaRun,
			ScopeDepsRun,
			ScopeSQLRun,
			ScopeCompileRun,
			ScopeSolveRun,
			ScopeTestRun,
			ScopeLintRun,
			ScopeAuditRun,
			ScopeBuildRun,
			ScopeBenchRun,
			ScopeBrowserRun,
			ScopeScaleData,
			ScopeScaleMedia,
		}
	default:
		return nil
	}
}

func scopeAliases(scope string) []string {
	switch scope {
	case ScopeDataRun, ScopeScaleData:
		return []string{ScopeDataRun, ScopeScaleData}
	case ScopeMediaRun, ScopeScaleMedia:
		return []string{ScopeMediaRun, ScopeScaleMedia}
	default:
		return []string{scope}
	}
}
