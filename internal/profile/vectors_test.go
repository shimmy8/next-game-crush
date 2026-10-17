package profile

import (
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestBuildVectorsUsesWeightedPositiveAndNegativeCentroids(t *testing.T) {
	a := make([]float32, Dimensions)
	b := make([]float32, Dimensions)
	a[0], b[1] = 1, 1
	result := BuildVectors([]Signal{
		{GameID: uuid.New(), Vector: a, PlaytimeMinutes: 1200, RecentMinutes: 120, AchievementRatio: .5, Wishlisted: true, Likes: 1},
		{GameID: uuid.New(), Vector: b, Dislikes: 2},
	})
	if result.Positive[0] != 1 || result.Positive[1] != 0 {
		t.Fatalf("positive centroid = [%v,%v], want [1,0]", result.Positive[0], result.Positive[1])
	}
	if result.Negative[0] != 0 || result.Negative[1] != 1 {
		t.Fatalf("negative centroid = [%v,%v], want [0,1]", result.Negative[0], result.Negative[1])
	}
	if result.Confidence <= 0 || result.Confidence > 1 {
		t.Fatalf("confidence = %v, want (0,1]", result.Confidence)
	}
}

func TestBuildVectorsHandlesSparseAndInvalidVectors(t *testing.T) {
	result := BuildVectors([]Signal{{Vector: nil, Wishlisted: true}, {Vector: []float32{1, 2}, Likes: 1}})
	if len(result.Positive) != Dimensions || len(result.Negative) != Dimensions || result.Confidence != 0 {
		t.Fatalf("sparse result = %#v", result)
	}
	for _, vector := range [][]float32{result.Positive, result.Negative} {
		for _, value := range vector {
			if value != 0 || math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				t.Fatalf("invalid sparse vector value %v", value)
			}
		}
	}
	if got := Cosine(result.Positive, result.Negative); got != 0 {
		t.Fatalf("zero-vector cosine = %v, want 0", got)
	}
}

func TestBuildVectorsIsDeterministicAcrossSignalOrder(t *testing.T) {
	v1, v2 := make([]float32, Dimensions), make([]float32, Dimensions)
	v1[0], v2[1] = 1, 1
	a := Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Vector: v1, PlaytimeMinutes: 10}
	b := Signal{GameID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Vector: v2, PlaytimeMinutes: 10}
	first, second := BuildVectors([]Signal{a, b}), BuildVectors([]Signal{b, a})
	if first.Positive[0] != second.Positive[0] || first.Positive[1] != second.Positive[1] {
		t.Fatalf("centroids differ by input order: %v vs %v", first.Positive[:2], second.Positive[:2])
	}
}
