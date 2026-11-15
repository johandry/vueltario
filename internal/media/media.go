package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/storage"
)

type ProbeResult struct {
	DurationMs *int64
	Width      *int
	Height     *int
}

type Service struct {
	cfg          config.Config
	store        *db.Store
	cloudStore   storage.ObjectStore
	transcodeSem chan struct{}
}

func NewService(cfg config.Config, store *db.Store) (*Service, error) {
	for _, sub := range []string{"quarantine", "blobs", "orphan_quarantine"} {
		if err := os.MkdirAll(filepath.Join(cfg.StorageRoot, sub), 0o750); err != nil {
			return nil, fmt.Errorf("init media dir %s: %w", sub, err)
		}
	}
	maxConc := cfg.MaxConcurrentTranscode
	if maxConc <= 0 {
		maxConc = 2
	}
	var cloud storage.ObjectStore
	switch cfg.StorageProvider {
	case "r2":
		if r2, err := storage.NewR2Store(storage.R2Config{
			AccountID:       cfg.R2AccountID,
			Endpoint:        cfg.R2Endpoint,
			Bucket:          cfg.R2Bucket,
			AccessKeyID:     cfg.R2AccessKeyID,
			SecretAccessKey: cfg.R2SecretAccessKey,
			PublicBaseURL:   cfg.R2PublicBaseURL,
		}); err == nil {
			cloud = r2
		}
	case "supabase":
		if sb, err := storage.NewSupabaseStorage(cfg.SupabaseURL, cfg.SupabaseServiceKey, cfg.SupabaseServiceKey, cfg.SupabaseBucket); err == nil {
			cloud = sb
		}
	}
	return &Service{
		cfg:          cfg,
		store:        store,
		cloudStore:   cloud,
		transcodeSem: make(chan struct{}, maxConc),
	}, nil
}

func (s *Service) CloudStore() storage.ObjectStore {
	return s.cloudStore
}

// ResolvePath strictly resolves a relative storage_key inside StorageRoot, preventing directory traversal.
func (s *Service) ResolvePath(storageKey string) (string, error) {
	if strings.TrimSpace(storageKey) == "" {
		return "", fmt.Errorf("%w: empty storage key", domain.ErrValidation)
	}
	rootAbs, err := filepath.Abs(s.cfg.StorageRoot)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(storageKey)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("%w: invalid storage key traversal", domain.ErrForbidden)
	}
	full := filepath.Join(rootAbs, cleaned)
	rel, err := filepath.Rel(rootAbs, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%w: storage key escapes root", domain.ErrForbidden)
	}
	return full, nil
}

type ImportRequest struct {
	User        domain.User
	SourceID    string
	Filename    string
	AccessScope string // defaults to "private"; "public_authorized" requires admin + rights
	Reader      io.Reader
}

// ImportAuthorizedFile implements Section 9 & MED-01/MED-02:
// quota check -> stream to quarantine with size ceiling -> sniff container magic bytes -> safe ffprobe ->
// duration ceiling check -> content-addressed promotion -> DB MediaAsset record.
func (s *Service) ImportAuthorizedFile(ctx context.Context, req ImportRequest) (*domain.MediaAsset, error) {
	if _, err := s.store.GetSource(ctx, req.SourceID); err != nil {
		return nil, err
	}
	usedBytes, err := s.store.GetUserStorageUsageBytes(ctx, req.User.ID)
	if err != nil {
		return nil, err
	}
	if s.cfg.AdminStorageQuotaBytes > 0 && usedBytes >= s.cfg.AdminStorageQuotaBytes {
		return nil, fmt.Errorf("%w: storage quota exceeded (%d / %d bytes)", domain.ErrQuotaExceeded, usedBytes, s.cfg.AdminStorageQuotaBytes)
	}

	scope := req.AccessScope
	if scope == "" {
		scope = "private"
	}
	if scope != "private" && req.User.Role != domain.RoleAdmin {
		return nil, fmt.Errorf("%w: only administrators may set non-private media scope", domain.ErrForbidden)
	}

	quarantineName := uuid.NewString() + ".part"
	quarantinePath := filepath.Join(s.cfg.StorageRoot, "quarantine", quarantineName)
	defer func() { _ = os.Remove(quarantinePath) }()

	f, err := os.OpenFile(quarantinePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, err
	}

	hasher := sha256.New()
	headerBuf := make([]byte, 64)
	limited := io.LimitReader(req.Reader, s.cfg.MaxUploadBytes+1)

	var totalBytes int64
	buf := make([]byte, 64*1024)
	for {
		n, readErr := limited.Read(buf)
		if n > 0 {
			if totalBytes < int64(len(headerBuf)) {
				copyLen := len(headerBuf) - int(totalBytes)
				if n < copyLen {
					copyLen = n
				}
				copy(headerBuf[totalBytes:], buf[:copyLen])
			}
			totalBytes += int64(n)
			if totalBytes > s.cfg.MaxUploadBytes {
				_ = f.Close()
				return nil, fmt.Errorf("%w: upload exceeds maximum size ceiling (%d bytes)", domain.ErrValidation, s.cfg.MaxUploadBytes)
			}
			if _, wErr := f.Write(buf[:n]); wErr != nil {
				_ = f.Close()
				return nil, wErr
			}
			_, _ = hasher.Write(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = f.Close()
			return nil, readErr
		}
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if totalBytes < 16 {
		return nil, fmt.Errorf("%w: uploaded media file is empty or truncated", domain.ErrValidation)
	}

	headerSlice := headerBuf
	if totalBytes < int64(len(headerBuf)) {
		headerSlice = headerBuf[:totalBytes]
	}
	mimeType, ext, ok := detectSupportedVideoContainer(headerSlice)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported or malformed video container (supported: MP4, MOV, WebM)", domain.ErrValidation)
	}

	probe := s.probeSafely(ctx, quarantinePath)
	if probe.DurationMs != nil && s.cfg.MaxClipDurationMs > 0 && *probe.DurationMs > s.cfg.MaxClipDurationMs {
		return nil, fmt.Errorf("%w: media duration (%d ms) exceeds maximum ceiling (%d ms)", domain.ErrValidation, *probe.DurationMs, s.cfg.MaxClipDurationMs)
	}

	shaHex := hex.EncodeToString(hasher.Sum(nil))
	relKey := filepath.ToSlash(filepath.Join("blobs", shaHex[:2], shaHex+ext))
	backend := "filesystem"

	if s.cloudStore != nil {
		rawBytes, rErr := os.ReadFile(quarantinePath)
		if rErr != nil {
			return nil, rErr
		}
		if uErr := s.cloudStore.Upload(ctx, relKey, mimeType, rawBytes); uErr != nil {
			return nil, uErr
		}
		backend = s.cloudStore.ProviderName()
	} else {
		destPath, err := s.ResolvePath(relKey)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
			return nil, err
		}
		if _, statErr := os.Stat(destPath); os.IsNotExist(statErr) {
			if err := os.Rename(quarantinePath, destPath); err != nil {
				return nil, err
			}
		}
	}

	asset, err := s.store.CreateMediaAsset(ctx, domain.MediaAsset{
		SourceID:       req.SourceID,
		OwnerUserID:    req.User.ID,
		AccessScope:    scope,
		AssetKind:      "original",
		StorageBackend: backend,
		StorageKey:     relKey,
		MIMEType:       mimeType,
		ByteSize:       totalBytes,
		SHA256Hex:      shaHex,
		DurationMs:     probe.DurationMs,
		Width:          probe.Width,
		Height:         probe.Height,
		Status:         "ready",
	})
	if err != nil {
		return nil, err
	}
	_ = s.store.RecordAudit(ctx, &req.User.ID, string(req.User.Role), "media.imported", "media_asset", asset.ID, 1, req.Filename, map[string]any{
		"sha256": shaHex,
		"bytes":  totalBytes,
		"mime":   mimeType,
	}, "")
	return asset, nil
}

// DeriveClip extracts a bounded segment from an authorized original asset using ffmpeg without invoking a shell.
func (s *Service) DeriveClip(ctx context.Context, user domain.User, parentAssetID string, startMs, endMs int64) (*domain.MediaAsset, error) {
	parent, err := s.store.GetMediaAsset(ctx, parentAssetID)
	if err != nil {
		return nil, err
	}
	if user.Role != domain.RoleAdmin && parent.OwnerUserID != user.ID {
		return nil, domain.ErrForbidden
	}
	if err := domain.ValidateOccurrenceBounds(&startMs, &endMs, parent.DurationMs); err != nil {
		return nil, err
	}
	if s.cfg.MaxClipDurationMs > 0 && (endMs-startMs) > s.cfg.MaxClipDurationMs {
		return nil, fmt.Errorf("%w: clip exceeds max duration ceiling", domain.ErrValidation)
	}
	srcPath, err := s.ResolvePath(parent.StorageKey)
	if err != nil {
		return nil, err
	}

	select {
	case s.transcodeSem <- struct{}{}:
		defer func() { <-s.transcodeSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	outTemp := filepath.Join(s.cfg.StorageRoot, "quarantine", uuid.NewString()+".mp4")
	defer func() { _ = os.Remove(outTemp) }()

	cmdCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	startSec := fmt.Sprintf("%.3f", float64(startMs)/1000.0)
	durSec := fmt.Sprintf("%.3f", float64(endMs-startMs)/1000.0)
	cmd := exec.CommandContext(cmdCtx, s.cfg.FFmpegBinary,
		"-nostdin", "-y",
		"-ss", startSec,
		"-i", srcPath,
		"-t", durSec,
		"-c", "copy",
		"-movflags", "+faststart",
		outTemp,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg clip derivation failed: %w (%s)", err, truncateStr(string(out), 200))
	}

	f, err := os.Open(outTemp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return nil, err
	}
	shaHex := hex.EncodeToString(h.Sum(nil))
	relKey := filepath.ToSlash(filepath.Join("blobs", shaHex[:2], shaHex+".mp4"))
	destPath, err := s.ResolvePath(relKey)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(destPath); os.IsNotExist(statErr) {
		if err := os.Rename(outTemp, destPath); err != nil {
			return nil, err
		}
	}
	clipDur := endMs - startMs
	parentID := parent.ID
	return s.store.CreateMediaAsset(ctx, domain.MediaAsset{
		SourceID:           parent.SourceID,
		OwnerUserID:        user.ID,
		AccessScope:        "private",
		AssetKind:          "clip_derivative",
		StorageBackend:     "filesystem",
		StorageKey:         relKey,
		MIMEType:           "video/mp4",
		ByteSize:           size,
		SHA256Hex:          shaHex,
		DurationMs:         &clipDur,
		Width:              parent.Width,
		Height:             parent.Height,
		Status:             "ready",
		DerivationParentID: &parentID,
		SourceTimeMap: &domain.SourceTimeMap{
			SourceStartMs: startMs,
			SourceEndMs:   endMs,
			ClipOffsetMs:  0,
		},
	})
}

func (s *Service) OpenPermittedStream(ctx context.Context, caller *domain.User, assetID string) (*os.File, *domain.MediaAsset, error) {
	asset, err := s.store.GetMediaAsset(ctx, assetID)
	if err != nil {
		return nil, nil, err
	}
	if asset.DeletedAt != nil || asset.Status != "ready" {
		return nil, nil, domain.ErrNotFound
	}
	if asset.AccessScope != "public_authorized" {
		if caller == nil {
			return nil, nil, domain.ErrUnauthorized
		}
		if caller.Role != domain.RoleAdmin && asset.OwnerUserID != caller.ID {
			return nil, nil, domain.ErrForbidden
		}
	}
	fullPath, err := s.ResolvePath(asset.StorageKey)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, domain.ErrNotFound
		}
		return nil, nil, err
	}
	return f, asset, nil
}

func (s *Service) DeleteAsset(ctx context.Context, caller domain.User, assetID string) error {
	storageKey, canPurgeFile, err := s.store.RevokeMediaAsset(ctx, caller, assetID)
	if err != nil {
		return err
	}
	if canPurgeFile && storageKey != "" {
		if fullPath, err := s.ResolvePath(storageKey); err == nil {
			_ = os.Remove(fullPath)
		}
	}
	_ = s.store.RecordAudit(ctx, &caller.ID, string(caller.Role), "media.deleted", "media_asset", assetID, 1, "", nil, "")
	return nil
}

// VerifyChecksum recalculates SHA-256 on disk and compares against the recorded MediaAsset checksum.
func (s *Service) VerifyChecksum(ctx context.Context, assetID string) (bool, error) {
	asset, err := s.store.GetMediaAsset(ctx, assetID)
	if err != nil {
		return false, err
	}
	fullPath, err := s.ResolvePath(asset.StorageKey)
	if err != nil {
		return false, err
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), asset.SHA256Hex), nil
}

// ScanOrphans implements Section 9 nightly orphan scan: moves unreferenced blobs into 7-day quarantine before purging.
func (s *Service) ScanOrphans(ctx context.Context) (quarantined int, purged int, err error) {
	activeKeys, err := s.store.AllActiveStorageKeys(ctx)
	if err != nil {
		return 0, 0, err
	}
	blobsRoot := filepath.Join(s.cfg.StorageRoot, "blobs")
	quarantineRoot := filepath.Join(s.cfg.StorageRoot, "orphan_quarantine")

	_ = filepath.Walk(blobsRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || info.IsDir() {
			return nil
		}
		rel, rErr := filepath.Rel(s.cfg.StorageRoot, path)
		if rErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if !activeKeys[relSlash] {
			qDest := filepath.Join(quarantineRoot, filepath.Base(path))
			if os.Rename(path, qDest) == nil {
				now := time.Now()
				_ = os.Chtimes(qDest, now, now)
				quarantined++
			}
		}
		return nil
	})

	cutoff := time.Now().Add(-s.cfg.OrphanQuarantineTTL)
	entries, _ := os.ReadDir(quarantineRoot)
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		info, iErr := ent.Info()
		if iErr == nil && info.ModTime().Before(cutoff) {
			if os.Remove(filepath.Join(quarantineRoot, ent.Name())) == nil {
				purged++
			}
		}
	}
	return quarantined, purged, nil
}

func (s *Service) probeSafely(ctx context.Context, filePath string) ProbeResult {
	if s.cfg.FFprobeBinary == "" {
		return ProbeResult{}
	}
	select {
	case s.transcodeSem <- struct{}{}:
		defer func() { <-s.transcodeSem }()
	case <-ctx.Done():
		return ProbeResult{}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Run ffprobe directly via exec.CommandContext (no shell!) with bounded timeout.
	cmd := exec.CommandContext(probeCtx, s.cfg.FFprobeBinary,
		"-v", "error",
		"-show_entries", "format=duration:stream=width,height,codec_type",
		"-of", "json",
		filePath,
	)
	out, err := cmd.Output()
	if err != nil {
		return ProbeResult{}
	}
	var parsed struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &parsed) != nil {
		return ProbeResult{}
	}
	var res ProbeResult
	if sec, err := strconv.ParseFloat(strings.TrimSpace(parsed.Format.Duration), 64); err == nil && sec >= 0 {
		ms := int64(sec * 1000)
		res.DurationMs = &ms
	}
	for _, st := range parsed.Streams {
		if st.CodecType == "video" && st.Width > 0 && st.Height > 0 {
			w, h := st.Width, st.Height
			res.Width = &w
			res.Height = &h
			break
		}
	}
	return res
}

func detectSupportedVideoContainer(hdr []byte) (mime string, ext string, ok bool) {
	if len(hdr) >= 12 && bytes.Equal(hdr[4:8], []byte("ftyp")) {
		brand := string(hdr[8:12])
		if brand == "qt  " {
			return "video/quicktime", ".mov", true
		}
		return "video/mp4", ".mp4", true
	}
	// WebM / Matroska EBML header 0x1A 0x45 0xDF 0xA3
	if len(hdr) >= 4 && hdr[0] == 0x1A && hdr[1] == 0x45 && hdr[2] == 0xDF && hdr[3] == 0xA3 {
		return "video/webm", ".webm", true
	}
	return "", "", false
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
