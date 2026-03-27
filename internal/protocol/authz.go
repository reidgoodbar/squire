package protocol

func TrustTierRank(tier string) int {
	switch tier {
	case TrustAdmin:
		return 4
	case TrustScaleAllowed:
		return 3
	case TrustTrusted:
		return 2
	case TrustGitHubBasic:
		return 1
	default:
		return 0
	}
}

func TrustTierAtLeast(actual, required string) bool {
	return TrustTierRank(actual) >= TrustTierRank(required)
}

func HasScope(scopes []string, required string) bool {
	for _, scope := range scopes {
		if scope == required || scope == ScopeAdmin {
			return true
		}
	}
	return false
}

func AllowedTokenScopesForTier(tier string) []string {
	switch tier {
	case TrustAdmin:
		return []string{ScopeUserRead, ScopeVerifyRun, ScopeScaleData, ScopeScaleMedia, ScopeAdmin}
	case TrustScaleAllowed:
		return []string{ScopeUserRead, ScopeVerifyRun, ScopeScaleData, ScopeScaleMedia}
	case TrustTrusted, TrustGitHubBasic:
		return []string{ScopeUserRead, ScopeVerifyRun, ScopeScaleData, ScopeScaleMedia}
	default:
		return nil
	}
}
