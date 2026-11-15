package aiassist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
)

// ExtractionResult represents the structured metadata and vuelta occurrences returned by an external or local AI agent.
type ExtractionResult struct {
	Title             *string               `json:"title"`
	CreatorName       *string               `json:"creator_name"`
	CreatorHandle     *string               `json:"creator_handle"`
	CreatorProfileURL *string               `json:"creator_profile_url"`
	CreatorRole       string                `json:"creator_role"`
	Duration          *string               `json:"duration"`
	DurationMs        *int64                `json:"duration_ms,omitempty"`
	PublicComment     *string               `json:"public_comment"`
	PrivateNotes      *string               `json:"private_notes"`
	Occurrences       []ExtractedOccurrence `json:"occurrences"`
}

type ExtractedOccurrence struct {
	MoveName  string  `json:"move_name"`
	MoveID    *string `json:"move_id,omitempty"`
	IsNewMove bool    `json:"is_new_move"`
	Start     *string `json:"start"`
	End       *string `json:"end"`
	StartMs   *int64  `json:"start_ms,omitempty"`
	EndMs     *int64  `json:"end_ms,omitempty"`
	Notes     string  `json:"notes"`
}

type PromptSourceInfo struct {
	EntryID      string   `json:"entry_id"`
	OriginalURL  string   `json:"original_url"`
	CanonicalURL string   `json:"canonical_url"`
	Platform     string   `json:"platform"`
	CurrentTitle string   `json:"current_title,omitempty"`
	ExtraContext string   `json:"extra_context,omitempty"`
	FramePaths   []string `json:"frame_paths,omitempty"`
}

type TaxonomyEntry struct {
	ID            string
	CanonicalName string
	SearchKey     string
	Aliases       []string
}

// BuildExtractionPrompt constructs the exact same taxonomy-grounded prompt used in `/admin`,
// enriched with any sampled local video frame paths when invoked from the CLI or Wails Desktop.
func BuildExtractionPrompt(src PromptSourceInfo, taxonomy []TaxonomyEntry) string {
	var taxLines []string
	for _, m := range taxonomy {
		if len(m.Aliases) > 0 {
			taxLines = append(taxLines, fmt.Sprintf("- %s (aliases: %s)", m.CanonicalName, strings.Join(m.Aliases, ", ")))
		} else {
			taxLines = append(taxLines, fmt.Sprintf("- %s", m.CanonicalName))
		}
	}
	taxBlock := "- (none yet)"
	if len(taxLines) > 0 {
		taxBlock = strings.Join(taxLines, "\n")
	}

	var extra strings.Builder
	if strings.TrimSpace(src.CurrentTitle) != "" {
		fmt.Fprintf(&extra, "Current Title: %s\n", strings.TrimSpace(src.CurrentTitle))
	}
	if len(src.FramePaths) > 0 {
		fmt.Fprintf(&extra, "\nSampled Local Video Frames (inspect on-screen text overlays, creator headers, and watermarks in these images):\n")
		for _, fp := range src.FramePaths {
			fmt.Fprintf(&extra, "- %s\n", fp)
		}
	}
	if strings.TrimSpace(src.ExtraContext) != "" {
		fmt.Fprintf(&extra, "\nAdditional Pasted Caption / Comments / Notes:\n\"\"\"\n%s\n\"\"\"\n", strings.TrimSpace(src.ExtraContext))
	} else if len(src.FramePaths) == 0 {
		extra.WriteString("\n(Tip: Attach screenshot(s) or sampled video frames so on-screen text overlays and creator headers can be read directly.)\n")
	}

	orig := src.OriginalURL
	if orig == "" {
		orig = src.CanonicalURL
	}

	return fmt.Sprintf(`You are assisting Vueltario, a curated catalog of Cuban dance moves (casino, Timba, and rueda de casino).
Extract accurate video metadata and all demonstrated dance moves (vueltas) for the following social video source.

Source URL: %s
Canonical URL: %s
Platform: %s
%s
Existing Vueltario Canonical Moves & Aliases (prefer these exact canonical Spanish names whenever a move or alias matches — for example, map "70" -> "Setenta", "Sácala" -> "Exhibela", "Adiós" -> "Prima"):
%s

Extraction Rules:
1. Identify the creator's display name, @handle, and profile URL from visible headers, watermarks, or captions. Set "creator_role" to "original" only if clearly the original dancers/choreographers, otherwise "uploader". Leave unknown fields as null (do NOT invent handles).
2. Extract every vuelta / move name from on-screen text overlays, spoken callouts, captions, or comments in the order they appear.
3. If an extracted move matches a canonical move or alias above, output the exact canonical_name and set "is_new_move": false. If it is a new vuelta or named combination not in the list above, keep its clean Spanish name and set "is_new_move": true.
4. Include "start" and "end" timestamps ("MM:SS" or "MM:SS.mmm") ONLY when actually observed or stated in the video/text; otherwise set them to null (never guess timestamps).
5. Reply ONLY with valid raw JSON matching this exact schema (no extra commentary):

{
  "title": "Descriptive Spanish or English video title or null",
  "creator_name": "Creator Display Name or null",
  "creator_handle": "handle_without_at or null",
  "creator_profile_url": "https://... or null",
  "creator_role": "uploader",
  "duration": "MM:SS or null",
  "public_comment": "Brief helpful summary of the lesson/social clip or null",
  "private_notes": "Any extra practice cues/uncertainties for the admin or null",
  "occurrences": [
    {
      "move_name": "Canonical or New Spanish Move Name",
      "is_new_move": false,
      "start": "00:12",
      "end": "00:22",
      "notes": "Optional cue (e.g. from on-screen text overlay)"
    }
  ]
}`, orig, src.CanonicalURL, src.Platform, extra.String(), taxBlock)
}

var codeFenceRe = regexp.MustCompile("(?is)```(?:json)?\\s*(\\{.*?\\})\\s*```")

// ParseAndValidateExtraction parses raw or markdown-fenced AI JSON output, maps aliases to canonical moves,
// and validates all timestamps against Vueltario's domain rules.
func ParseAndValidateExtraction(raw string, taxonomy []TaxonomyEntry) (*ExtractionResult, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty AI JSON payload", domain.ErrValidation)
	}

	candidate := trimmed
	if m := codeFenceRe.FindStringSubmatch(trimmed); len(m) == 2 {
		candidate = strings.TrimSpace(m[1])
	} else if start := strings.IndexByte(trimmed, '{'); start >= 0 {
		if end := strings.LastIndexByte(trimmed, '}'); end > start {
			candidate = trimmed[start : end+1]
		}
	}

	var parsed ExtractionResult
	if err := json.Unmarshal([]byte(candidate), &parsed); err != nil {
		return nil, fmt.Errorf("%w: invalid AI JSON: %v", domain.ErrValidation, err)
	}

	parsed.Title = cleanPtr(parsed.Title)
	parsed.CreatorName = cleanPtr(parsed.CreatorName)
	if h := cleanPtr(parsed.CreatorHandle); h != nil {
		cleanH := strings.TrimPrefix(*h, "@")
		parsed.CreatorHandle = &cleanH
	} else {
		parsed.CreatorHandle = nil
	}
	parsed.CreatorProfileURL = cleanPtr(parsed.CreatorProfileURL)
	parsed.PublicComment = cleanPtr(parsed.PublicComment)
	parsed.PrivateNotes = cleanPtr(parsed.PrivateNotes)
	if parsed.CreatorRole != "original" && parsed.CreatorRole != "collaborator" {
		parsed.CreatorRole = "uploader"
	}

	if d := cleanPtr(parsed.Duration); d != nil {
		ms, err := domain.ParseTimestampMs(*d)
		if err != nil {
			return nil, fmt.Errorf("invalid AI duration: %w", err)
		}
		parsed.DurationMs = ms
	}

	var validOccs []ExtractedOccurrence
	for _, occ := range parsed.Occurrences {
		name := strings.TrimSpace(occ.MoveName)
		if name == "" {
			continue
		}
		norm := domain.NormalizeSearchKey(name)
		var matched *TaxonomyEntry
		for i := range taxonomy {
			if taxonomy[i].SearchKey == norm {
				matched = &taxonomy[i]
				break
			}
			for _, al := range taxonomy[i].Aliases {
				if domain.NormalizeSearchKey(al) == norm {
					matched = &taxonomy[i]
					break
				}
			}
			if matched != nil {
				break
			}
		}

		var sMs, eMs *int64
		if st := cleanPtr(occ.Start); st != nil {
			p, err := domain.ParseTimestampMs(*st)
			if err != nil {
				return nil, fmt.Errorf("occurrence %q start: %w", name, err)
			}
			sMs = p
		}
		if en := cleanPtr(occ.End); en != nil {
			p, err := domain.ParseTimestampMs(*en)
			if err != nil {
				return nil, fmt.Errorf("occurrence %q end: %w", name, err)
			}
			eMs = p
		}
		if err := domain.ValidateOccurrenceBounds(sMs, eMs, parsed.DurationMs); err != nil {
			return nil, fmt.Errorf("occurrence %q: %w", name, err)
		}

		if matched != nil {
			idCopy := matched.ID
			occ.MoveID = &idCopy
			occ.MoveName = matched.CanonicalName
			occ.IsNewMove = false
		} else {
			occ.MoveID = nil
			occ.MoveName = name
			occ.IsNewMove = true
		}
		occ.StartMs = sMs
		occ.EndMs = eMs
		occ.Notes = strings.TrimSpace(occ.Notes)
		validOccs = append(validOccs, occ)
	}
	parsed.Occurrences = validOccs
	return &parsed, nil
}

// SampleVideoFrames extracts evenly spaced JPEG frames from a local video file via ffmpeg (no shell invoked!)
// so a local multimodal CLI/Desktop agent (Claude Code, Copilot CLI, Ollama llava/qwen-vl, etc.) can OCR on-screen vuelta titles.
func SampleVideoFrames(ctx context.Context, ffmpegBin, videoPath, outDir string, intervalSec int, maxFrames int) ([]string, error) {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	if intervalSec <= 0 {
		intervalSec = 4
	}
	if maxFrames <= 0 || maxFrames > 24 {
		maxFrames = 8
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return nil, err
	}

	outPattern := filepath.Join(outDir, "frame_%02d.jpg")
	vfExpr := fmt.Sprintf("fps=1/%d,scale='min(720,iw)':-2", intervalSec)

	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, ffmpegBin,
		"-nostdin", "-y",
		"-i", videoPath,
		"-vf", vfExpr,
		"-frames:v", fmt.Sprintf("%d", maxFrames),
		"-q:v", "4",
		outPattern,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg frame sampling failed: %w (%s)", err, truncateString(string(out), 180))
	}

	matches, err := filepath.Glob(filepath.Join(outDir, "frame_*.jpg"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// RunLocalAgentCommand pipes the generated prompt on stdin to an explicit local agent executable + args (no shell!)
// and returns its stdout so ParseAndValidateExtraction can validate and apply the JSON.
func RunLocalAgentCommand(ctx context.Context, prompt string, executable string, args []string) (string, error) {
	if strings.TrimSpace(executable) == "" {
		return "", fmt.Errorf("%w: agent executable is required", domain.ErrValidation)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, executable, args...)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("local agent %q failed: %w (%s)", executable, err, truncateString(stderr.String(), 200))
	}
	return stdout.String(), nil
}

// --- Local SQL Store Application ---

func LoadTaxonomyFromStore(ctx context.Context, store *db.Store) ([]TaxonomyEntry, error) {
	moves, err := store.ListMoves(ctx, "", true)
	if err != nil {
		return nil, err
	}
	out := make([]TaxonomyEntry, 0, len(moves))
	for _, m := range moves {
		var aliases []string
		for _, al := range m.Aliases {
			aliases = append(aliases, al.Alias)
		}
		out = append(out, TaxonomyEntry{
			ID:            m.ID,
			CanonicalName: m.CanonicalName,
			SearchKey:     m.NormalizedSearchKey,
			Aliases:       aliases,
		})
	}
	return out, nil
}

func ApplyToStore(ctx context.Context, store *db.Store, user domain.User, entryID string, res *ExtractionResult, replaceOccurrences bool) (*domain.CatalogEntry, error) {
	ent, err := store.GetEntry(ctx, user.ID, entryID)
	if err != nil {
		return nil, err
	}

	var creatorIn *db.CreatorAttributionInput
	if res.CreatorName != nil && *res.CreatorName != "" {
		handle := ""
		if res.CreatorHandle != nil {
			handle = *res.CreatorHandle
		}
		prof := ""
		if res.CreatorProfileURL != nil {
			prof = *res.CreatorProfileURL
		}
		creatorIn = &db.CreatorAttributionInput{
			DisplayName: *res.CreatorName,
			Role:        res.CreatorRole,
			Handle:      handle,
			ProfileURL:  prof,
			Verified:    false,
		}
	}

	notes := ent.PrivateNotes
	if res.PrivateNotes != nil && *res.PrivateNotes != "" {
		if notes != "" {
			notes = notes + "\n[AI]: " + *res.PrivateNotes
		} else {
			notes = *res.PrivateNotes
		}
	}

	updated, err := store.UpdateEntry(ctx, db.UpdateEntryInput{
		UserID:                user.ID,
		EntryID:               ent.ID,
		ExpectedRevision:      ent.Revision,
		PrivateNotes:          &notes,
		PersonalTitleOverride: res.Title,
		SourceDurationMs:      res.DurationMs,
		CreatorAttribution:    creatorIn,
	})
	if err != nil {
		return nil, err
	}

	if replaceOccurrences && len(res.Occurrences) > 0 {
		for _, existing := range updated.Occurrences {
			_ = store.DeleteOccurrence(ctx, user.ID, ent.ID, existing.ID)
		}
	}

	for idx, occ := range res.Occurrences {
		moveName := occ.MoveName
		method := domain.TimingUnknown
		if occ.StartMs != nil {
			method = domain.TimingSuggested
		}
		if _, err := store.AddOccurrence(ctx, db.OccurrenceInput{
			UserID:       user.ID,
			EntryID:      ent.ID,
			MoveID:       occ.MoveID,
			ProposedName: &moveName,
			StartMs:      occ.StartMs,
			EndMs:        occ.EndMs,
			Ordinal:      idx + 1,
			Notes:        occ.Notes,
			State:        domain.OccurrenceConfirmed,
			TimingMethod: method,
		}); err != nil {
			return nil, err
		}
	}

	return store.GetEntry(ctx, user.ID, ent.ID)
}

// --- Direct Remote Supabase PostgREST Client (So `vueltario ai --supabase` works directly with GitHub Pages DB!) ---

type SupabaseRemote struct {
	baseURL string
	apiKey  string
	bearer  string
	client  *http.Client
}

func NewSupabaseRemote(cfg config.Config) (*SupabaseRemote, error) {
	if cfg.SupabaseURL == "" {
		return nil, fmt.Errorf("VUELTARIO_SUPABASE_URL is not configured in environment/.env")
	}
	apiKey := cfg.SupabaseServiceKey
	if apiKey == "" {
		apiKey = cfg.SupabaseAnonKey
	}
	if apiKey == "" {
		return nil, fmt.Errorf("VUELTARIO_SUPABASE_SERVICE_KEY (or VUELTARIO_SUPABASE_ANON_KEY) is required")
	}
	return &SupabaseRemote{
		baseURL: strings.TrimRight(cfg.SupabaseURL, "/"),
		apiKey:  apiKey,
		bearer:  apiKey,
		client:  &http.Client{Timeout: 25 * time.Second},
	}, nil
}

func (sr *SupabaseRemote) LoadPromptContext(ctx context.Context, entryIDOrURL string) (PromptSourceInfo, []TaxonomyEntry, error) {
	var moves []struct {
		ID            string `json:"id"`
		CanonicalName string `json:"canonical_name"`
		SearchKey     string `json:"normalized_search_key"`
		Aliases       []struct {
			Alias string `json:"alias"`
		} `json:"move_aliases"`
	}
	if err := sr.getJSON(ctx, "/rest/v1/moves?select=id,canonical_name,normalized_search_key,move_aliases(alias)&order=canonical_name", &moves); err != nil {
		return PromptSourceInfo{}, nil, err
	}
	tax := make([]TaxonomyEntry, 0, len(moves))
	for _, m := range moves {
		var al []string
		for _, a := range m.Aliases {
			al = append(al, a.Alias)
		}
		tax = append(tax, TaxonomyEntry{ID: m.ID, CanonicalName: m.CanonicalName, SearchKey: m.SearchKey, Aliases: al})
	}

	var entries []struct {
		ID           string  `json:"id"`
		Platform     string  `json:"platform"`
		CanonicalURL string  `json:"canonical_url"`
		OriginalURL  string  `json:"original_url"`
		Title        *string `json:"title"`
	}
	filter := "id=eq." + url.QueryEscape(entryIDOrURL)
	if strings.HasPrefix(entryIDOrURL, "http://") || strings.HasPrefix(entryIDOrURL, "https://") {
		filter = fmt.Sprintf("or=(canonical_url.eq.%s,original_url.eq.%s)", url.QueryEscape(entryIDOrURL), url.QueryEscape(entryIDOrURL))
	}
	if err := sr.getJSON(ctx, "/rest/v1/entries?select=id,platform,canonical_url,original_url,title&limit=1&"+filter, &entries); err != nil {
		return PromptSourceInfo{}, nil, err
	}
	if len(entries) == 0 {
		return PromptSourceInfo{}, nil, fmt.Errorf("%w: Supabase entry %q not found", domain.ErrNotFound, entryIDOrURL)
	}
	ent := entries[0]
	title := ""
	if ent.Title != nil {
		title = *ent.Title
	}
	return PromptSourceInfo{
		EntryID:      ent.ID,
		OriginalURL:  ent.OriginalURL,
		CanonicalURL: ent.CanonicalURL,
		Platform:     ent.Platform,
		CurrentTitle: title,
	}, tax, nil
}

func (sr *SupabaseRemote) ApplyExtraction(ctx context.Context, entryID string, platform string, res *ExtractionResult, replaceOccurrences bool) error {
	var creatorID *string
	if res.CreatorName != nil && *res.CreatorName != "" {
		cid, err := sr.ensureCreator(ctx, *res.CreatorName, platform, res.CreatorHandle, res.CreatorProfileURL)
		if err != nil {
			return err
		}
		creatorID = &cid
	}

	patch := map[string]any{}
	if res.Title != nil {
		patch["title"] = *res.Title
	}
	if creatorID != nil {
		patch["creator_id"] = *creatorID
		patch["creator_role"] = res.CreatorRole
	}
	if res.DurationMs != nil {
		patch["duration_ms"] = *res.DurationMs
	}
	if res.PublicComment != nil {
		patch["public_comment"] = *res.PublicComment
	}
	if len(patch) > 0 {
		if err := sr.doMutate(ctx, http.MethodPatch, "/rest/v1/entries?id=eq."+url.QueryEscape(entryID), patch, nil); err != nil {
			return err
		}
	}

	if res.PrivateNotes != nil && *res.PrivateNotes != "" {
		_ = sr.doMutate(ctx, http.MethodPost, "/rest/v1/admin_entry_notes", map[string]any{
			"entry_id":      entryID,
			"private_notes": *res.PrivateNotes,
		}, map[string]string{"Prefer": "resolution=merge-duplicates,return=minimal"})
	}

	if replaceOccurrences && len(res.Occurrences) > 0 {
		if err := sr.doMutate(ctx, http.MethodDelete, "/rest/v1/occurrences?entry_id=eq."+url.QueryEscape(entryID), nil, nil); err != nil {
			return err
		}
	}

	for idx, occ := range res.Occurrences {
		moveID := occ.MoveID
		if moveID == nil {
			mid, err := sr.ensureMove(ctx, occ.MoveName)
			if err != nil {
				return err
			}
			moveID = &mid
		}
		method := "unknown"
		if occ.StartMs != nil {
			method = "suggested"
		}
		var prop *string
		if occ.IsNewMove {
			n := occ.MoveName
			prop = &n
		}
		if err := sr.doMutate(ctx, http.MethodPost, "/rest/v1/occurrences", map[string]any{
			"entry_id":      entryID,
			"move_id":       moveID,
			"proposed_name": prop,
			"start_ms":      occ.StartMs,
			"end_ms":        occ.EndMs,
			"ordinal":       idx + 1,
			"notes":         occ.Notes,
			"state":         "confirmed",
			"timing_method": method,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (sr *SupabaseRemote) ensureCreator(ctx context.Context, name, platform string, handle, profileURL *string) (string, error) {
	norm := domain.NormalizeSearchKey(name)
	var found []struct {
		ID string `json:"id"`
	}
	_ = sr.getJSON(ctx, "/rest/v1/creators?select=id&limit=1&normalized_name=eq."+url.QueryEscape(norm), &found)
	var creatorID string
	if len(found) > 0 {
		creatorID = found[0].ID
	} else {
		var created []struct {
			ID string `json:"id"`
		}
		if err := sr.postReturnJSON(ctx, "/rest/v1/creators", map[string]any{
			"display_name":    name,
			"normalized_name": norm,
		}, &created); err != nil || len(created) == 0 {
			return "", fmt.Errorf("create Supabase creator failed: %v", err)
		}
		creatorID = created[0].ID
	}

	if (handle != nil && *handle != "") || (profileURL != nil && *profileURL != "") {
		prof := ""
		if profileURL != nil {
			prof = *profileURL
		}
		_ = sr.doMutate(ctx, http.MethodPost, "/rest/v1/creator_identities", map[string]any{
			"creator_id":  creatorID,
			"platform":    platform,
			"handle":      handle,
			"profile_url": prof,
		}, nil)
	}
	return creatorID, nil
}

func (sr *SupabaseRemote) ensureMove(ctx context.Context, name string) (string, error) {
	norm := domain.NormalizeSearchKey(name)
	var found []struct {
		ID string `json:"id"`
	}
	_ = sr.getJSON(ctx, "/rest/v1/moves?select=id&limit=1&normalized_search_key=eq."+url.QueryEscape(norm), &found)
	if len(found) > 0 {
		return found[0].ID, nil
	}
	var created []struct {
		ID string `json:"id"`
	}
	err := sr.postReturnJSON(ctx, "/rest/v1/moves", map[string]any{
		"canonical_name":        name,
		"normalized_search_key": norm,
		"portable_token":        domain.PortableMoveToken(name, ""),
		"style_tags":            []string{"casino"},
		"kind":                  "move",
		"curation_state":        "curated",
	}, &created)
	if err != nil || len(created) == 0 {
		return "", fmt.Errorf("create Supabase move failed: %v", err)
	}
	return created[0].ID, nil
}

func (sr *SupabaseRemote) getJSON(ctx context.Context, relPath string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sr.baseURL+relPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", sr.apiKey)
	req.Header.Set("Authorization", "Bearer "+sr.bearer)
	resp, err := sr.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("supabase GET %s HTTP %d: %s", relPath, resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func (sr *SupabaseRemote) postReturnJSON(ctx context.Context, relPath string, body any, dst any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sr.baseURL+relPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("apikey", sr.apiKey)
	req.Header.Set("Authorization", "Bearer "+sr.bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=representation")
	resp, err := sr.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("supabase POST %s HTTP %d: %s", relPath, resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func (sr *SupabaseRemote) doMutate(ctx context.Context, method, relPath string, body any, extraHeaders map[string]string) error {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, sr.baseURL+relPath, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", sr.apiKey)
	req.Header.Set("Authorization", "Bearer "+sr.bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Prefer", "return=minimal")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := sr.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("supabase %s %s HTTP %d: %s", method, relPath, resp.StatusCode, string(b))
	}
	return nil
}

func cleanPtr(p *string) *string {
	if p == nil {
		return nil
	}
	s := strings.TrimSpace(*p)
	if s == "" {
		return nil
	}
	return &s
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
