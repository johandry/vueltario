package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/publishing"
)

type cliFakeIG struct {
	published int
}

func (f *cliFakeIG) CreateReelContainer(ctx context.Context, videoStagingURL, caption string) (string, error) {
	return "cli-container-1", nil
}
func (f *cliFakeIG) GetContainerStatus(ctx context.Context, containerID string) (publishing.ContainerStatus, error) {
	return publishing.ContainerStatus{ContainerID: containerID, StatusCode: "FINISHED"}, nil
}
func (f *cliFakeIG) PublishContainer(ctx context.Context, containerID string) (string, string, error) {
	f.published++
	return "cli-media-1", "https://www.instagram.com/reel/CLIReel1/", nil
}
func (f *cliFakeIG) PostFirstComment(ctx context.Context, mediaID, commentText string) (string, error) {
	return "cli-comment-1", nil
}

func TestCLIEndToEndIngestCatalogMediaAndPublish(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := config.Load()
	cfg.DBDriver = "sqlite"
	cfg.DatabaseURL = filepath.Join(tmp, "cli.db")
	cfg.StorageRoot = filepath.Join(tmp, "storage")
	cfg.FFprobeBinary = ""

	fakeIG := &cliFakeIG{}
	runCLI := func(args ...string) (int, string, string) {
		var out, errBuf bytes.Buffer
		app := &App{Cfg: cfg, Stdout: &out, Stderr: &errBuf, Transport: fakeIG}
		code := app.Run(ctx, args)
		return code, out.String(), errBuf.String()
	}

	// 1. Batch ingest with 2 valid + 1 unsupported URL (ING-05: does not abort batch)
	batchPath := filepath.Join(tmp, "urls.txt")
	_ = os.WriteFile(batchPath, []byte("https://youtu.be/dQw4w9WgXcQ\nhttps://vimeo.com/999\nhttps://www.instagram.com/reel/IGTest99/\n"), 0o600)
	code, out, errStr := runCLI("ingest", "--batch", batchPath, "--json")
	if code != ExitOK {
		t.Fatalf("batch ingest failed (%d): %s", code, errStr)
	}
	var batchRes struct {
		Created int `json:"created"`
		Failed  int `json:"failed"`
		Items   []struct {
			EntryID string `json:"entry_id"`
			Status  string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &batchRes); err != nil {
		t.Fatalf("unmarshal batch: %v", err)
	}
	if batchRes.Created != 2 || batchRes.Failed != 1 {
		t.Fatalf("expected 2 created & 1 failed, got %+v", batchRes)
	}
	ytEntryID := batchRes.Items[0].EntryID

	// 2. Add occurrence via CLI
	code, _, errStr = runCLI("catalog", "add-occurrence", "--entry", ytEntryID, "--move", "Sombrero", "--start", "00:10", "--end", "00:18")
	if code != ExitOK {
		t.Fatalf("add-occurrence failed: %s", errStr)
	}

	// Lookup source_id via catalog show --json
	code, showOut, _ := runCLI("catalog", "show", "--json", ytEntryID)
	if code != ExitOK {
		t.Fatalf("catalog show failed")
	}
	var shown struct {
		SourceID string `json:"source_id"`
	}
	_ = json.Unmarshal([]byte(showOut), &shown)

	// 3. Import authorized MP4 and record video+music rights
	mp4Path := filepath.Join(tmp, "authorized.mp4")
	_ = os.WriteFile(mp4Path, []byte{
		0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm',
		0x00, 0x00, 0x02, 0x00, 'i', 's', 'o', 'm', 'm', 'p', '4', '2',
	}, 0o600)
	code, impOut, errStr := runCLI("media", "import", "--source", shown.SourceID, "--file", mp4Path, "--json")
	if code != ExitOK {
		t.Fatalf("media import failed: %s", errStr)
	}
	var asset struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(impOut), &asset)

	code, _, errStr = runCLI("media", "rights-add", "--source", shown.SourceID, "--asset", asset.ID, "--grantor", "Creator", "--basis", "Email OK", "--video-cleared", "--music-cleared")
	if code != ExitOK {
		t.Fatalf("rights-add failed: %s", errStr)
	}

	// 4. Publish draft -> approve wrong hash fails -> approve exact hash -> dry-run -> live run
	code, draftOut, errStr := runCLI("publish", "draft", "--entry", ytEntryID, "--asset", asset.ID, "--companion-tags", "--json")
	if code != ExitOK {
		t.Fatalf("publish draft failed: %s", errStr)
	}
	var draft struct {
		ID        string `json:"id"`
		DraftHash string `json:"draft_hash"`
	}
	_ = json.Unmarshal([]byte(draftOut), &draft)

	// Wrong hash MUST exit with ExitConflict (4)
	if badCode, _, _ := runCLI("publish", "approve", "--hash", "wronghash", draft.ID); badCode != ExitConflict {
		t.Fatalf("expected ExitConflict (4) for mismatched hash, got %d", badCode)
	}

	if code, _, errStr = runCLI("publish", "approve", "--hash", draft.DraftHash, "--account", "@cubansocial.sd", draft.ID); code != ExitOK {
		t.Fatalf("publish approve failed: %s", errStr)
	}

	// Dry-run must not publish remotely
	if code, _, errStr = runCLI("publish", "run", "--dry-run", draft.ID); code != ExitOK {
		t.Fatalf("publish dry-run failed: %s", errStr)
	}
	if fakeIG.published != 0 {
		t.Fatalf("dry-run published remotely!")
	}

	// Live run publishes exactly once
	code, pubOut, errStr := runCLI("publish", "run", "--idempotency-key", "cli-run-1", "--json", draft.ID)
	if code != ExitOK {
		t.Fatalf("publish run failed: %s", errStr)
	}
	var published struct {
		Status    string `json:"status"`
		ResultURL string `json:"result_url"`
	}
	_ = json.Unmarshal([]byte(pubOut), &published)
	if published.Status != "published" || published.ResultURL == "" || fakeIG.published != 1 {
		t.Fatalf("unexpected publish outcome: %+v (calls=%d)", published, fakeIG.published)
	}
}
