package recommendations

import (
	"testing"

	"github.com/google/uuid"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
)

func TestRankingBuildKeyIncludesAlgorithmVersion(t *testing.T) {
	source := RankingSource{Activity: profile.Profile{ID: uuid.New(), Version: 2}, ProfileAlgorithmVersion: "activity-real", ProfileBuildKey: "profile-build", ProfileInputContentHash: "profile-input", Overlay: preferences.Overlay{ID: uuid.New(), Version: 3, AlgorithmVersion: "overlay-real", BuildKey: "overlay-build", InputContentHash: "overlay-input"}}
	if got := RankingBuildKey("same-workflow", AlgorithmVersion, source); rankingRequestPrefix(got) == "" {
		t.Fatalf("build key = %q", got)
	}
}
