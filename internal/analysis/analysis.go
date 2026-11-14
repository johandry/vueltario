package analysis

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/providers"
)

const ProcessorVersion = "vueltario-lexicon-v1.0"

// TranscriptSegment represents an optional authorized transcript snippet.
type TranscriptSegment struct {
	Text    string
	StartMs int64
	EndMs   int64
}

type TranscriptProvider interface {
	TranscribeAuthorizedAsset(ctx context.Context, asset domain.MediaAsset) ([]TranscriptSegment, error)
}

type OCRSegment struct {
	Text       string
	ObservedMs int64
}

type OCRProvider interface {
	ExtractOverlayText(ctx context.Context, asset domain.MediaAsset) ([]OCRSegment, error)
}

type Service struct {
	cfg         config.Config
	store       *db.Store
	providers   *providers.Registry
	transcripts TranscriptProvider
	ocr         OCRProvider
}

func NewService(cfg config.Config, store *db.Store, reg *providers.Registry, tp TranscriptProvider, ocr OCRProvider) *Service {
	return &Service{
		cfg:         cfg,
		store:       store,
		providers:   reg,
		transcripts: tp,
		ocr:         ocr,
	}
}

type AnalysisOutcome struct {
	EvidenceCount   int                 `json:"evidence_count"`
	SuggestionCount int                 `json:"suggestion_count"`
	Suggestions     []domain.Suggestion `json:"suggestions"`
}

// AnalyzeEntry implements ANA-01..ANA-03:
// - Respects global AnalysisEnabled and user AnalysisOptOut
// - Enforces daily analysis budget ceiling & skips redundant reanalysis of unchanged source revisions when suggestions already exist
// - Collects evidence from accessible title, description, permitted comments, and optional authorized transcript/OCR adapters
// - Distinguishes announcement timestamps (single point) from movement ranges (start–end)
// - Preserves confirmed/rejected user occurrences and applies the >=90% precision release gate
func (s *Service) AnalyzeEntry(ctx context.Context, user domain.User, entryID string, force bool) (*AnalysisOutcome, error) {
	if !s.cfg.AnalysisEnabled {
		return nil, fmt.Errorf("%w: assisted analysis is disabled by configuration", providers.ErrCapabilityUnsupported)
	}
	if user.AnalysisOptOut {
		return nil, fmt.Errorf("%w: user has opted out of assisted analysis", domain.ErrForbidden)
	}

	ent, err := s.store.GetEntry(ctx, user.ID, entryID)
	if err != nil {
		return nil, err
	}

	// Cost control (Section 14): do not repeat analysis on unchanged source revision unless explicitly forced.
	if !force {
		for _, sg := range ent.Suggestions {
			if sg.SourceRevision == ent.Source.MetadataVersion && sg.ProcessorVersion == ProcessorVersion {
				return &AnalysisOutcome{
					EvidenceCount:   len(ent.Evidence),
					SuggestionCount: len(ent.Suggestions),
					Suggestions:     ent.Suggestions,
				}, nil
			}
		}
	}

	if err := s.store.TryConsumeAnalysisBudget(ctx, s.cfg.AnalysisDailyBudget); err != nil {
		return nil, err
	}

	moves, err := s.store.ListMoves(ctx, "", true)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	retention := now.Add(s.cfg.AIIntermediateTTL)
	var evidences []domain.Evidence

	if ent.Source.Title != nil && strings.TrimSpace(*ent.Source.Title) != "" {
		evidences = append(evidences, domain.Evidence{
			ID:          uuid.NewString(),
			SourceID:    ent.SourceID,
			Kind:        "title",
			OriginalRef: ent.Source.CanonicalURL,
			Excerpt:     truncate(*ent.Source.Title, 300),
			AuthorRole:  "creator",
			AccessScope: "public_factual",
			RetrievedAt: now,
		})
	}
	if ent.Source.Description != nil && strings.TrimSpace(*ent.Source.Description) != "" {
		evidences = append(evidences, domain.Evidence{
			ID:          uuid.NewString(),
			SourceID:    ent.SourceID,
			Kind:        "description",
			OriginalRef: ent.Source.CanonicalURL,
			Excerpt:     truncate(*ent.Source.Description, 800),
			AuthorRole:  "creator",
			AccessScope: "public_factual",
			RetrievedAt: now,
		})
	}

	// Permitted comments excerpt (Section 13: collect only small evidence excerpts, honor retention deadline)
	if adapter, ok := s.providers.Get(ent.Source.Platform); ok {
		if comments, cErr := adapter.FetchComments(ctx, ent.Source, 10); cErr == nil {
			for _, c := range comments {
				role := "viewer"
				if c.IsCreator {
					role = "creator"
				}
				evidences = append(evidences, domain.Evidence{
					ID:                uuid.NewString(),
					SourceID:          ent.SourceID,
					Kind:              "comment",
					OriginalRef:       c.CommentID,
					Excerpt:           truncate(c.Text, 350),
					AuthorRole:        role,
					AccessScope:       "public_factual",
					RetrievedAt:       now,
					RetentionDeadline: &retention,
				})
			}
		}
	}

	// Optional transcript/OCR on user's private/authorized asset (never makes private asset public!)
	if len(ent.Assets) > 0 {
		asset := ent.Assets[0]
		uid := user.ID
		if s.transcripts != nil {
			if segs, tErr := s.transcripts.TranscribeAuthorizedAsset(ctx, asset); tErr == nil {
				for _, seg := range segs {
					sMs, eMs := seg.StartMs, seg.EndMs
					evidences = append(evidences, domain.Evidence{
						ID:                uuid.NewString(),
						SourceID:          ent.SourceID,
						OwnerUserID:       &uid,
						Kind:              "transcript",
						OriginalRef:       asset.ID,
						Excerpt:           truncate(seg.Text, 300),
						ObservedStartMs:   &sMs,
						ObservedEndMs:     &eMs,
						IsAnnouncementTS:  true, // Spoken callout precedes or coincides with announcement
						AuthorRole:        "instructor_audio",
						AccessScope:       "private_user",
						RetrievedAt:       now,
						RetentionDeadline: &retention,
					})
				}
			}
		}
		if s.ocr != nil {
			if overlays, oErr := s.ocr.ExtractOverlayText(ctx, asset); oErr == nil {
				for _, ov := range overlays {
					obs := ov.ObservedMs
					evidences = append(evidences, domain.Evidence{
						ID:                uuid.NewString(),
						SourceID:          ent.SourceID,
						OwnerUserID:       &uid,
						Kind:              "ocr",
						OriginalRef:       asset.ID,
						Excerpt:           truncate(ov.Text, 200),
						ObservedStartMs:   &obs,
						IsAnnouncementTS:  true,
						AuthorRole:        "video_overlay",
						AccessScope:       "private_user",
						RetrievedAt:       now,
						RetentionDeadline: &retention,
					})
				}
			}
		}
	}

	suggestions := ExtractSuggestionsFromEvidence(ent.ID, ent.Source.MetadataVersion, ent.Source.DurationMs, moves, evidences, s.cfg.AnalysisHighConfVerified)

	if err := s.store.SaveAnalysisResults(ctx, user.ID, ent.ID, ent.Source.MetadataVersion, evidences, suggestions); err != nil {
		return nil, err
	}
	updated, err := s.store.GetEntry(ctx, user.ID, ent.ID)
	if err != nil {
		return nil, err
	}
	return &AnalysisOutcome{
		EvidenceCount:   len(updated.Evidence),
		SuggestionCount: len(updated.Suggestions),
		Suggestions:     updated.Suggestions,
	}, nil
}

type lexiconTerm struct {
	MoveID        string
	CanonicalName string
	NormPhrase    string
	TokenCount    int
	Colliding     bool
}

var (
	rangeTSPattern  = regexp.MustCompile(`(\d{1,2}:\d{2}(?::\d{2})?(?:\.\d{1,3})?)\s*(?:[-–—]|to|a)\s*(\d{1,2}:\d{2}(?::\d{2})?(?:\.\d{1,3})?)\s*[:\-–]?\s*([^\n\r;|]+)`)
	singleTSPattern = regexp.MustCompile(`(\d{1,2}:\d{2}(?::\d{2})?(?:\.\d{1,3})?)\s*[:\-–]?\s*([^\n\r;|]+)`)
)

// ExtractSuggestionsFromEvidence matches curated/proposed moves and aliases against evidence text,
// preferring longer multi-word move phrases (e.g., "Enchufla doble" over "Enchufla") and distinguishing
// range timestamps from single announcement timestamps (ANA-03).
func ExtractSuggestionsFromEvidence(entryID string, sourceRevision int, sourceDurationMs *int64, moves []domain.Move, evidences []domain.Evidence, verifiedBenchmarkGatePassed bool) []domain.Suggestion {
	var terms []lexiconTerm
	for _, m := range moves {
		if m.CurationState == "rejected" || m.NormalizedSearchKey == "" {
			continue
		}
		terms = append(terms, lexiconTerm{
			MoveID:        m.ID,
			CanonicalName: m.CanonicalName,
			NormPhrase:    m.NormalizedSearchKey,
			TokenCount:    len(strings.Fields(m.NormalizedSearchKey)),
		})
		for _, al := range m.Aliases {
			if al.NormalizedAlias == "" {
				continue
			}
			terms = append(terms, lexiconTerm{
				MoveID:        m.ID,
				CanonicalName: m.CanonicalName,
				NormPhrase:    al.NormalizedAlias,
				TokenCount:    len(strings.Fields(al.NormalizedAlias)),
				Colliding:     al.CollisionFlagged,
			})
		}
	}
	// Longest phrase first so "Enchufla doble" claims its span before "Enchufla"
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].TokenCount != terms[j].TokenCount {
			return terms[i].TokenCount > terms[j].TokenCount
		}
		return len(terms[i].NormPhrase) > len(terms[j].NormPhrase)
	})

	type candidateKey struct {
		moveID  string
		startMs int64
	}
	grouped := make(map[candidateKey]*domain.Suggestion)
	var order []candidateKey

	addCandidate := func(term lexiconTerm, ev domain.Evidence, startMs, endMs *int64, announcementOnly bool) {
		if startMs != nil && endMs != nil {
			if domain.ValidateOccurrenceBounds(startMs, endMs, sourceDurationMs) != nil {
				startMs, endMs = nil, nil
			}
		} else if startMs != nil && sourceDurationMs != nil && *sourceDurationMs > 0 && *startMs > *sourceDurationMs {
			startMs = nil
		}

		var keyStart int64 = -1
		if startMs != nil {
			keyStart = *startMs
		}
		k := candidateKey{moveID: term.MoveID, startMs: keyStart}
		if existing, ok := grouped[k]; ok {
			if !containsStr(existing.EvidenceIDs, ev.ID) {
				existing.EvidenceIDs = append(existing.EvidenceIDs, ev.ID)
			}
			return
		}

		// Confidence band strictly honors Section 16 release gate:
		// If held-out benchmark >=90% precision has NOT been established, label "unverified" requiring confirmation.
		band := "unverified"
		if verifiedBenchmarkGatePassed && !term.Colliding && (ev.Kind == "title" || ev.Kind == "description" || ev.Kind == "ocr") {
			band = "high"
		} else if verifiedBenchmarkGatePassed {
			band = "medium"
		}

		mid := term.MoveID
		sg := &domain.Suggestion{
			ID:                 uuid.NewString(),
			EntryID:            entryID,
			SourceRevision:     sourceRevision,
			CandidateMoveID:    &mid,
			CandidateName:      term.CanonicalName,
			StartMs:            startMs,
			EndMs:              endMs,
			IsAnnouncementOnly: announcementOnly,
			EvidenceIDs:        []string{ev.ID},
			ProcessorVersion:   ProcessorVersion,
			ConfidenceBand:     band,
			State:              "pending",
		}
		grouped[k] = sg
		order = append(order, k)
	}

	for _, ev := range evidences {
		// 1. Check explicit range lines first: "00:12 - 00:22 Enchufla"
		matchedLineSpans := make([]string, 0)
		for _, rm := range rangeTSPattern.FindAllStringSubmatch(ev.Excerpt, -1) {
			sMs, err1 := domain.ParseTimestampMs(rm[1])
			eMs, err2 := domain.ParseTimestampMs(rm[2])
			if err1 == nil && err2 == nil && sMs != nil && eMs != nil && *eMs > *sMs {
				matchedLineSpans = append(matchedLineSpans, rm[0])
				for _, t := range matchTermsInText(rm[3], terms) {
					addCandidate(t, ev, sMs, eMs, false)
				}
			}
		}

		// 2. Check single announcement timestamps: "00:12 Enchufla" (marked IsAnnouncementOnly=true per ANA-03!)
		remaining := ev.Excerpt
		for _, span := range matchedLineSpans {
			remaining = strings.ReplaceAll(remaining, span, " ")
		}
		for _, sm := range singleTSPattern.FindAllStringSubmatch(remaining, -1) {
			sMs, err := domain.ParseTimestampMs(sm[1])
			if err == nil && sMs != nil {
				for _, t := range matchTermsInText(sm[2], terms) {
					addCandidate(t, ev, sMs, nil, true)
				}
			}
		}

		// 3. General text mentions (or transcript/OCR observed timestamp)
		for _, t := range matchTermsInText(ev.Excerpt, terms) {
			// Skip if we already captured a timed candidate for this exact move from this evidence
			alreadyTimed := false
			for _, k := range order {
				if k.moveID == t.MoveID && k.startMs >= 0 && containsStr(grouped[k].EvidenceIDs, ev.ID) {
					alreadyTimed = true
					break
				}
			}
			if alreadyTimed {
				continue
			}
			addCandidate(t, ev, ev.ObservedStartMs, ev.ObservedEndMs, ev.IsAnnouncementTS)
		}
	}

	out := make([]domain.Suggestion, 0, len(order))
	for _, k := range order {
		out = append(out, *grouped[k])
	}
	return out
}

func matchTermsInText(rawText string, sortedTerms []lexiconTerm) []lexiconTerm {
	padded := " " + domain.NormalizeSearchKey(rawText) + " "
	if strings.TrimSpace(padded) == "" {
		return nil
	}
	var matched []lexiconTerm
	seenMove := make(map[string]bool)
	working := padded
	for _, t := range sortedTerms {
		needle := " " + t.NormPhrase + " "
		if strings.Contains(working, needle) {
			if !seenMove[t.MoveID] {
				seenMove[t.MoveID] = true
				matched = append(matched, t)
			}
			// Mask matched phrase so shorter sub-phrases ("Enchufla" inside "Enchufla doble") don't falsely double-trigger.
			working = strings.ReplaceAll(working, needle, " ")
		}
	}
	return matched
}

// --- Section 16 Benchmark Evaluator ---

type BenchmarkClip struct {
	ClipID          string
	CreatorID       string
	ExpectedMoves   []string
	ExpectedRanges  map[string][2]int64
	Evidences       []domain.Evidence
	SourceDuration  *int64
}

type BenchmarkReport struct {
	TotalClips             int     `json:"total_clips"`
	PredictedCount         int     `json:"predicted_count"`
	TruePositives          int     `json:"true_positives"`
	GroundTruthCount       int     `json:"ground_truth_count"`
	AbstainedClips         int     `json:"abstained_clips"`
	Precision              float64 `json:"precision"`
	Recall                 float64 `json:"recall"`
	AbstentionRate         float64 `json:"abstention_rate"`
	BoundariesWithin2Sec   float64 `json:"boundaries_within_2s_ratio"`
	MeetsNameReleaseGate   bool    `json:"meets_name_release_gate"`   // >= 0.90 precision
	MeetsTimingReleaseGate bool    `json:"meets_timing_release_gate"` // >= 0.80 within 2000ms
}

func EvaluateBenchmark(clips []BenchmarkClip, moves []domain.Move) (BenchmarkReport, error) {
	if len(clips) == 0 {
		return BenchmarkReport{}, errors.New("benchmark requires at least one clip")
	}
	var rep BenchmarkReport
	rep.TotalClips = len(clips)
	var boundaryTotal, boundaryWithin2s int

	for _, clip := range clips {
		suggs := ExtractSuggestionsFromEvidence(clip.ClipID, 1, clip.SourceDuration, moves, clip.Evidences, true)
		if len(suggs) == 0 {
			rep.AbstainedClips++
		}
		truthSet := make(map[string]bool)
		for _, m := range clip.ExpectedMoves {
			truthSet[domain.NormalizeSearchKey(m)] = true
			rep.GroundTruthCount++
		}
		predSeen := make(map[string]bool)
		for _, sg := range suggs {
			nk := domain.NormalizeSearchKey(sg.CandidateName)
			if !predSeen[nk] {
				predSeen[nk] = true
				rep.PredictedCount++
				if truthSet[nk] {
					rep.TruePositives++
				}
			}
			if expRange, ok := clip.ExpectedRanges[nk]; ok && sg.StartMs != nil && sg.EndMs != nil && !sg.IsAnnouncementOnly {
				boundaryTotal += 2
				if math.Abs(float64(*sg.StartMs-expRange[0])) <= 2000 {
					boundaryWithin2s++
				}
				if math.Abs(float64(*sg.EndMs-expRange[1])) <= 2000 {
					boundaryWithin2s++
				}
			}
		}
	}

	if rep.PredictedCount > 0 {
		rep.Precision = float64(rep.TruePositives) / float64(rep.PredictedCount)
	}
	if rep.GroundTruthCount > 0 {
		rep.Recall = float64(rep.TruePositives) / float64(rep.GroundTruthCount)
	}
	rep.AbstentionRate = float64(rep.AbstainedClips) / float64(rep.TotalClips)
	if boundaryTotal > 0 {
		rep.BoundariesWithin2Sec = float64(boundaryWithin2s) / float64(boundaryTotal)
	}
	rep.MeetsNameReleaseGate = rep.PredictedCount > 0 && rep.Precision >= 0.90
	rep.MeetsTimingReleaseGate = boundaryTotal > 0 && rep.BoundariesWithin2Sec >= 0.80
	return rep, nil
}

func containsStr(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

func truncate(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
