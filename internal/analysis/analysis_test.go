package analysis

import (
	"testing"
	"time"

	"github.com/johandry/vueltario/internal/domain"
)

func TestExtractSuggestionsDistinguishesRangeVsAnnouncementAndBenchmarkGate(t *testing.T) {
	moves := []domain.Move{
		{ID: "m1", CanonicalName: "Enchufla", NormalizedSearchKey: "enchufla", CurationState: "curated"},
		{ID: "m2", CanonicalName: "Enchufla doble", NormalizedSearchKey: "enchufla doble", CurationState: "curated"},
		{ID: "m3", CanonicalName: "Dile que no", NormalizedSearchKey: "dile que no", CurationState: "curated"},
	}
	dur := int64(120000)
	evidences := []domain.Evidence{
		{
			ID:          "ev1",
			Kind:        "description",
			Excerpt:     "00:12 - 00:22 Enchufla doble\n00:25 Dile que no",
			AuthorRole:  "creator",
			AccessScope: "public_factual",
			RetrievedAt: time.Now().UTC(),
		},
	}

	// Before held-out benchmark gate passes (verifiedBenchmarkGatePassed=false), all suggestions MUST be labeled "unverified"
	unverified := ExtractSuggestionsFromEvidence("ent1", 1, &dur, moves, evidences, false)
	if len(unverified) != 2 {
		t.Fatalf("expected 2 suggestions (Enchufla doble must not double-trigger plain Enchufla), got %d: %+v", len(unverified), unverified)
	}
	if unverified[0].CandidateName != "Enchufla doble" || unverified[0].IsAnnouncementOnly || unverified[0].ConfidenceBand != "unverified" {
		t.Fatalf("unexpected range suggestion: %+v", unverified[0])
	}
	if unverified[1].CandidateName != "Dile que no" || !unverified[1].IsAnnouncementOnly || unverified[1].EndMs != nil {
		t.Fatalf("single timestamp must be flagged IsAnnouncementOnly=true without fabricated end_ms: %+v", unverified[1])
	}

	// Verify Section 16 benchmark evaluator
	rep, err := EvaluateBenchmark([]BenchmarkClip{
		{
			ClipID:         "c1",
			CreatorID:      "cr1",
			ExpectedMoves:  []string{"Enchufla doble", "Dile que no"},
			ExpectedRanges: map[string][2]int64{"enchufla doble": {11500, 22500}},
			Evidences:      evidences,
			SourceDuration: &dur,
		},
	}, moves)
	if err != nil {
		t.Fatalf("benchmark error: %v", err)
	}
	if !rep.MeetsNameReleaseGate || !rep.MeetsTimingReleaseGate {
		t.Fatalf("expected benchmark gates to pass, got %+v", rep)
	}
}
