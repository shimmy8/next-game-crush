package recommendations

import "testing"

func TestRankingBuildKeyIncludesAlgorithmVersion(t *testing.T) {
	if got := RankingBuildKey("same-workflow", AlgorithmVersion, ActiveAdjustmentsDigest(nil)); rankingRequestPrefix(got) == "" {
		t.Fatalf("build key = %q", got)
	}
}
