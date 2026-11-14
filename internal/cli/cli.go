package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/johandry/vueltario/internal/analysis"
	"github.com/johandry/vueltario/internal/api"
	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/media"
	"github.com/johandry/vueltario/internal/netguard"
	"github.com/johandry/vueltario/internal/providers"
	"github.com/johandry/vueltario/internal/publishing"
	"github.com/johandry/vueltario/internal/worker"
)

const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitBlocked  = 3
	ExitConflict = 4
)

type App struct {
	Cfg       config.Config
	Stdout    io.Writer
	Stderr    io.Writer
	Transport publishing.Transport // Optional override for CLI integration tests
}

func (a *App) Run(ctx context.Context, args []string) int {
	if a.Stdout == nil {
		a.Stdout = os.Stdout
	}
	if a.Stderr == nil {
		a.Stderr = os.Stderr
	}
	if err := a.Cfg.Validate(); err != nil {
		fmt.Fprintf(a.Stderr, "configuration error: %v\n", err)
		return ExitUsage
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		a.printUsage()
		return ExitOK
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "serve":
		return a.cmdServe(ctx, rest)
	case "worker":
		return a.cmdWorker(ctx, rest)
	case "migrate":
		return a.cmdMigrate(ctx, rest)
	case "capabilities":
		return a.cmdCapabilities(rest)
	case "ingest":
		return a.cmdIngest(ctx, rest)
	case "catalog":
		return a.cmdCatalog(ctx, rest)
	case "media":
		return a.cmdMedia(ctx, rest)
	case "publish":
		return a.cmdPublish(ctx, rest)
	default:
		fmt.Fprintf(a.Stderr, "unknown command %q\n\n", sub)
		a.printUsage()
		return ExitUsage
	}
}

func (a *App) printUsage() {
	fmt.Fprint(a.Stdout, `Vueltario — Cuban Dance Move Catalog & Publishing CLI

Usage:
  vueltario <command> [subcommand] [flags]

Commands:
  serve           Start HTTP API and responsive Vanilla JS website (--with-worker for dev)
  worker          Start durable background enrichment & maintenance worker
  migrate         Run database schema migrations (up | down)
  capabilities    Display Phase 0 provider capability contracts & test fixtures
  ingest          Save a single social video link or batch file (--batch <path>)
  catalog         Manage personal/public catalog (list, show, moves, add-occurrence, export, backup, restore)
  media           Manage authorized media & rights (import, rights-add, rights-revoke, orphan-scan)
  publish         Instagram @cubansocial.sd workflow (draft, preview, approve, run, status, reconcile)
`)
}

type runtimeStack struct {
	store     *db.Store
	providers *providers.Registry
	mediaSvc  *media.Service
	analyzer  *analysis.Service
	pubSvc    *publishing.Service
	hub       *worker.EventHub
	admin     *domain.User
}

func (a *App) openStack(ctx context.Context) (*runtimeStack, error) {
	store, err := db.Open(a.Cfg.DBDriver, a.Cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := store.MigrateUp(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	admin, err := store.EnsureBootstrap(ctx, a.Cfg.AdminEmail, a.Cfg.AdminPassword)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if admin == nil && a.Cfg.AdminEmail != "" {
		admin, _ = store.GetUserByEmail(ctx, a.Cfg.AdminEmail)
	}
	safeHTTP := netguard.NewSafeClient(netguard.Options{})
	reg := providers.NewDefaultRegistry(safeHTTP, a.Cfg.YouTubeAPIKey)
	mediaSvc, err := media.NewService(a.Cfg, store)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	analyzer := analysis.NewService(a.Cfg, store, reg, nil, nil)
	pubSvc := publishing.NewService(a.Cfg, store, a.Transport)
	hub := worker.NewEventHub()

	return &runtimeStack{
		store:     store,
		providers: reg,
		mediaSvc:  mediaSvc,
		analyzer:  analyzer,
		pubSvc:    pubSvc,
		hub:       hub,
		admin:     admin,
	}, nil
}

func (a *App) cmdServe(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	addr := fs.String("addr", a.Cfg.Addr, "HTTP listen address")
	withWorker := fs.Bool("with-worker", false, "Run background job worker in the same process")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	stack, err := a.openStack(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "startup failed: %v\n", err)
		return ExitError
	}
	defer stack.store.Close()

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *withWorker {
		runner := worker.NewRunner(a.Cfg, stack.store, stack.providers, stack.analyzer, stack.mediaSvc, stack.hub)
		go func() {
			_ = runner.Run(runCtx)
		}()
	}

	srv := api.NewServer(a.Cfg, stack.store, stack.providers, stack.mediaSvc, stack.analyzer, stack.pubSvc, stack.hub)
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-runCtx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	fmt.Fprintf(a.Stdout, "Vueltario listening on %s (db=%s, worker=%v)\n", *addr, stack.store.Dialect(), *withWorker)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(a.Stderr, "server error: %v\n", err)
		return ExitError
	}
	return ExitOK
}

func (a *App) cmdWorker(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	once := fs.Bool("once", false, "Process queued jobs until idle and exit")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	stack, err := a.openStack(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "worker startup failed: %v\n", err)
		return ExitError
	}
	defer stack.store.Close()

	runner := worker.NewRunner(a.Cfg, stack.store, stack.providers, stack.analyzer, stack.mediaSvc, stack.hub)
	if *once {
		for {
			ok, err := runner.ProcessOne(ctx)
			if err != nil {
				fmt.Fprintf(a.Stderr, "worker error: %v\n", err)
				return ExitError
			}
			if !ok {
				return ExitOK
			}
		}
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(a.Stdout, "Vueltario worker %s started (db=%s)\n", runner.ID, stack.store.Dialect())
	if err := runner.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(a.Stderr, "worker stopped: %v\n", err)
		return ExitError
	}
	return ExitOK
}

func (a *App) cmdMigrate(ctx context.Context, args []string) int {
	dir := "up"
	if len(args) > 0 {
		dir = args[0]
	}
	store, err := db.Open(a.Cfg.DBDriver, a.Cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(a.Stderr, "db open error: %v\n", err)
		return ExitError
	}
	defer store.Close()

	switch dir {
	case "up":
		if err := store.MigrateUp(ctx); err != nil {
			fmt.Fprintf(a.Stderr, "migrate up failed: %v\n", err)
			return ExitError
		}
		fmt.Fprintf(a.Stdout, "Migrations applied (%s)\n", store.Dialect())
	case "down":
		if err := store.MigrateDown(ctx); err != nil {
			fmt.Fprintf(a.Stderr, "migrate down failed: %v\n", err)
			return ExitError
		}
		fmt.Fprintf(a.Stdout, "Migrations rolled back (%s)\n", store.Dialect())
	default:
		fmt.Fprintf(a.Stderr, "expected 'up' or 'down', got %q\n", dir)
		return ExitUsage
	}
	return ExitOK
}

func (a *App) cmdCapabilities(args []string) int {
	fs := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	asJSON := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	reg := providers.NewDefaultRegistry(nil, a.Cfg.YouTubeAPIKey)
	caps := reg.AllCapabilities()
	if *asJSON {
		return a.writeJSONOut(map[string]any{
			"capabilities": caps,
			"fixtures":     providers.RepresentativeFixtures(),
		})
	}
	for _, c := range caps {
		fmt.Fprintf(a.Stdout, "%-10s  embed=%-12s  time/seek=%-22s  metadata=%-12s\n",
			c.Platform, c.WebEmbeddedPlayback, c.TimeAndSeek, c.PublicMetadata)
	}
	return ExitOK
}

func (a *App) cmdIngest(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	batchFile := fs.String("batch", "", "Path to newline-delimited URLs file (ING-05)")
	notes := fs.String("notes", "", "Private practice notes")
	asJSON := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	stack, err := a.openStack(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		return ExitError
	}
	defer stack.store.Close()
	if stack.admin == nil {
		fmt.Fprintln(a.Stderr, "error: bootstrap admin account is required")
		return ExitBlocked
	}

	if *batchFile != "" {
		f, err := os.Open(*batchFile)
		if err != nil {
			fmt.Fprintf(a.Stderr, "cannot open batch file: %v\n", err)
			return ExitUsage
		}
		defer f.Close()

		type batchItem struct {
			URL     string `json:"url"`
			Status  string `json:"status"`
			EntryID string `json:"entry_id,omitempty"`
			Error   string `json:"error,omitempty"`
		}
		var items []batchItem
		var created, dups, failed int
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			res, sErr := stack.store.SaveLink(ctx, db.SaveLinkRequest{
				UserID:       stack.admin.ID,
				RawURL:       line,
				PrivateNotes: *notes,
			})
			if sErr != nil {
				failed++
				items = append(items, batchItem{URL: line, Status: "failed", Error: sErr.Error()})
				continue
			}
			st := "created"
			if res.Deduplicated {
				st = "duplicate"
				dups++
			} else {
				created++
			}
			items = append(items, batchItem{URL: line, Status: st, EntryID: res.Entry.ID})
		}
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(a.Stderr, "batch read error: %v\n", err)
			return ExitError
		}
		if *asJSON {
			return a.writeJSONOut(map[string]any{
				"created": created, "duplicates": dups, "failed": failed, "items": items,
			})
		}
		fmt.Fprintf(a.Stdout, "Batch complete: %d created, %d duplicates, %d failed\n", created, dups, failed)
		for _, it := range items {
			if it.Error != "" {
				fmt.Fprintf(a.Stdout, "  [FAIL] %s — %s\n", it.URL, it.Error)
			} else {
				fmt.Fprintf(a.Stdout, "  [%s] %s -> %s\n", strings.ToUpper(it.Status), it.URL, it.EntryID)
			}
		}
		return ExitOK
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(a.Stderr, "usage: vueltario ingest [--notes ...] [--json] <url>")
		return ExitUsage
	}
	res, err := stack.store.SaveLink(ctx, db.SaveLinkRequest{
		UserID:       stack.admin.ID,
		RawURL:       fs.Arg(0),
		PrivateNotes: *notes,
	})
	if err != nil {
		return a.mapError(err)
	}
	if *asJSON {
		return a.writeJSONOut(res)
	}
	fmt.Fprintf(a.Stdout, "Saved entry %s (%s) [deduplicated=%v]\n", res.Entry.ID, res.Source.Platform, res.Deduplicated)
	return ExitOK
}

func (a *App) cmdCatalog(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Stderr, "usage: vueltario catalog <list|show|moves|add-occurrence|export|backup|restore> [flags]")
		return ExitUsage
	}
	sub := args[0]
	rest := args[1:]

	stack, err := a.openStack(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		return ExitError
	}
	defer stack.store.Close()

	switch sub {
	case "list":
		fs := flag.NewFlagSet("catalog list", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		q := fs.String("q", "", "Search query")
		platform := fs.String("platform", "", "Platform filter")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil {
			return ExitUsage
		}
		entries, err := stack.store.ListEntries(ctx, db.EntryFilter{
			UserID:   stack.admin.ID,
			Query:    *q,
			Platform: *platform,
		})
		if err != nil {
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(entries)
		}
		for _, e := range entries {
			fmt.Fprintf(a.Stdout, "%s  %-10s  rev=v%-2d  occs=%-2d  %s\n",
				e.ID, e.Source.Platform, e.Revision, len(e.Occurrences), e.Source.CanonicalURL)
		}
		return ExitOK

	case "show":
		fs := flag.NewFlagSet("catalog show", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || fs.NArg() < 1 {
			fmt.Fprintln(a.Stderr, "usage: vueltario catalog show [--json] <entry_id>")
			return ExitUsage
		}
		ent, err := stack.store.GetEntry(ctx, stack.admin.ID, fs.Arg(0))
		if err != nil {
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(ent)
		}
		fmt.Fprintf(a.Stdout, "Entry:   %s (v%d, %s)\nURL:     %s\nPlatform:%s\n",
			ent.ID, ent.Revision, ent.VisibilityState, ent.Source.CanonicalURL, ent.Source.Platform)
		for _, o := range ent.Occurrences {
			fmt.Fprintf(a.Stdout, "  - %s [%s -> %s] (%s)\n",
				o.MoveName, domain.FormatTimestampMs(o.StartMs, true), domain.FormatTimestampMs(o.EndMs, true), o.TimingMethod)
		}
		return ExitOK

	case "moves":
		fs := flag.NewFlagSet("catalog moves", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		q := fs.String("q", "", "Search moves or aliases")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil {
			return ExitUsage
		}
		moves, err := stack.store.ListMoves(ctx, *q, true)
		if err != nil {
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(moves)
		}
		for _, m := range moves {
			fmt.Fprintf(a.Stdout, "%s  %-26s  #CubanSocial-%s\n", m.ID, m.CanonicalName, m.PortableToken)
		}
		return ExitOK

	case "add-occurrence":
		fs := flag.NewFlagSet("catalog add-occurrence", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		entryID := fs.String("entry", "", "Entry ID")
		moveName := fs.String("move", "", "Move name")
		startStr := fs.String("start", "", "Start timestamp (MM:SS.mmm or sec)")
		endStr := fs.String("end", "", "End timestamp (MM:SS.mmm or sec)")
		notes := fs.String("notes", "", "Notes")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || *entryID == "" || *moveName == "" {
			fmt.Fprintln(a.Stderr, "usage: vueltario catalog add-occurrence --entry <id> --move <name> [--start 00:12] [--end 00:22] [--json]")
			return ExitUsage
		}
		sMs, err := domain.ParseTimestampMs(*startStr)
		if err != nil {
			return a.mapError(err)
		}
		eMs, err := domain.ParseTimestampMs(*endStr)
		if err != nil {
			return a.mapError(err)
		}
		occ, err := stack.store.AddOccurrence(ctx, db.OccurrenceInput{
			UserID:       stack.admin.ID,
			EntryID:      *entryID,
			ProposedName: moveName,
			StartMs:      sMs,
			EndMs:        eMs,
			Notes:        *notes,
		})
		if err != nil {
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(occ)
		}
		fmt.Fprintf(a.Stdout, "Added occurrence %s (%s)\n", occ.ID, occ.MoveName)
		return ExitOK

	case "export":
		bundle, err := stack.store.ExportPersonalCatalog(ctx, stack.admin.ID)
		if err != nil {
			return a.mapError(err)
		}
		return a.writeJSONOut(bundle)

	case "backup":
		fs := flag.NewFlagSet("catalog backup", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		outPath := fs.String("out", "", "Output JSON path (stdout if empty)")
		if err := fs.Parse(rest); err != nil {
			return ExitUsage
		}
		bundle, err := stack.store.CreateBackupBundle(ctx, stack.admin.ID)
		if err != nil {
			return a.mapError(err)
		}
		raw, _ := json.MarshalIndent(bundle, "", "  ")
		if *outPath != "" {
			if err := os.WriteFile(*outPath, raw, 0o600); err != nil {
				fmt.Fprintf(a.Stderr, "write backup failed: %v\n", err)
				return ExitError
			}
			fmt.Fprintf(a.Stdout, "Backup written to %s (%d entries)\n", *outPath, len(bundle.Entries))
			return ExitOK
		}
		fmt.Fprintln(a.Stdout, string(raw))
		return ExitOK

	case "restore":
		fs := flag.NewFlagSet("catalog restore", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		inPath := fs.String("in", "", "Input backup JSON path")
		if err := fs.Parse(rest); err != nil || *inPath == "" {
			fmt.Fprintln(a.Stderr, "usage: vueltario catalog restore --in <backup.json>")
			return ExitUsage
		}
		raw, err := os.ReadFile(*inPath)
		if err != nil {
			fmt.Fprintf(a.Stderr, "read backup failed: %v\n", err)
			return ExitError
		}
		var bundle db.BackupBundle
		if err := json.Unmarshal(raw, &bundle); err != nil {
			fmt.Fprintf(a.Stderr, "invalid backup JSON: %v\n", err)
			return ExitUsage
		}
		n, err := stack.store.RestoreBackupBundle(ctx, stack.admin.ID, bundle)
		if err != nil {
			return a.mapError(err)
		}
		fmt.Fprintf(a.Stdout, "Restored %d catalog entries\n", n)
		return ExitOK

	default:
		fmt.Fprintf(a.Stderr, "unknown catalog subcommand %q\n", sub)
		return ExitUsage
	}
}

func (a *App) cmdMedia(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Stderr, "usage: vueltario media <import|rights-add|rights-revoke|orphan-scan> [flags]")
		return ExitUsage
	}
	sub := args[0]
	rest := args[1:]

	stack, err := a.openStack(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		return ExitError
	}
	defer stack.store.Close()

	switch sub {
	case "import":
		fs := flag.NewFlagSet("media import", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		sourceID := fs.String("source", "", "Source ID")
		filePath := fs.String("file", "", "Path to authorized MP4/MOV/WebM file")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || *sourceID == "" || *filePath == "" {
			fmt.Fprintln(a.Stderr, "usage: vueltario media import --source <source_id> --file <path> [--json]")
			return ExitUsage
		}
		f, err := os.Open(*filePath)
		if err != nil {
			fmt.Fprintf(a.Stderr, "cannot open file: %v\n", err)
			return ExitUsage
		}
		defer f.Close()
		asset, err := stack.mediaSvc.ImportAuthorizedFile(ctx, media.ImportRequest{
			User:     *stack.admin,
			SourceID: *sourceID,
			Filename: *filePath,
			Reader:   f,
		})
		if err != nil {
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(asset)
		}
		fmt.Fprintf(a.Stdout, "Imported media asset %s (SHA-256: %s, %d bytes)\n", asset.ID, asset.SHA256Hex, asset.ByteSize)
		return ExitOK

	case "rights-add":
		fs := flag.NewFlagSet("media rights-add", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		sourceID := fs.String("source", "", "Source ID")
		assetID := fs.String("asset", "", "Optional Asset ID")
		grantor := fs.String("grantor", "", "Creator or rights grantor")
		basis := fs.String("basis", "", "Permission basis / written reference")
		videoOK := fs.Bool("video-cleared", false, "Video redistribution cleared")
		musicOK := fs.Bool("music-cleared", false, "Music rights cleared")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || *sourceID == "" || *grantor == "" || *basis == "" {
			fmt.Fprintln(a.Stderr, "usage: vueltario media rights-add --source <id> --grantor <name> --basis <ref> --video-cleared --music-cleared [--json]")
			return ExitUsage
		}
		var aPtr *string
		if *assetID != "" {
			aPtr = assetID
		}
		rec, err := stack.store.CreateRightsRecord(ctx, domain.RightsRecord{
			SourceID:            *sourceID,
			AssetID:             aPtr,
			Grantor:             *grantor,
			Basis:               *basis,
			VideoCleared:        *videoOK,
			MusicCleared:        *musicOK,
			AuthorizedUses:      []string{"republish"},
			AuthorizedPlatforms: []string{"instagram"},
			ReviewerID:          stack.admin.ID,
		})
		if err != nil {
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(rec)
		}
		fmt.Fprintf(a.Stdout, "Recorded rights %s (video=%v, music=%v)\n", rec.ID, rec.VideoCleared, rec.MusicCleared)
		return ExitOK

	case "rights-revoke":
		fs := flag.NewFlagSet("media rights-revoke", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		rightsID := fs.String("id", "", "Rights Record ID")
		reason := fs.String("reason", "Revoked by administrator", "Revocation reason")
		if err := fs.Parse(rest); err != nil || *rightsID == "" {
			fmt.Fprintln(a.Stderr, "usage: vueltario media rights-revoke --id <rights_id> [--reason ...]")
			return ExitUsage
		}
		if err := stack.store.RevokeRightsRecord(ctx, stack.admin.ID, *rightsID, *reason); err != nil {
			return a.mapError(err)
		}
		fmt.Fprintf(a.Stdout, "Revoked rights %s and blocked pending publications\n", *rightsID)
		return ExitOK

	case "orphan-scan":
		q, p, err := stack.mediaSvc.ScanOrphans(ctx)
		if err != nil {
			return a.mapError(err)
		}
		fmt.Fprintf(a.Stdout, "Orphan scan complete: %d quarantined, %d expired blobs purged\n", q, p)
		return ExitOK

	default:
		fmt.Fprintf(a.Stderr, "unknown media subcommand %q\n", sub)
		return ExitUsage
	}
}

func (a *App) cmdPublish(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Stderr, "usage: vueltario publish <draft|preview|approve|run|status|reconcile> [flags]")
		return ExitUsage
	}
	sub := args[0]
	rest := args[1:]

	stack, err := a.openStack(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		return ExitError
	}
	defer stack.store.Close()

	switch sub {
	case "draft":
		fs := flag.NewFlagSet("publish draft", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		entryID := fs.String("entry", "", "Admin private entry ID")
		pubRevID := fs.String("public-revision", "", "Approved public revision ID")
		assetID := fs.String("asset", "", "Authorized media asset ID")
		account := fs.String("account", a.Cfg.IGDestination, "Destination Instagram account")
		brand := fs.String("brand", a.Cfg.IGBrandPrefix, "Brand tag prefix")
		companion := fs.Bool("companion-tags", false, "Explicitly append compatible underscore companion hashtags (#Brand_Move)")
		tagsInComment := fs.Bool("tags-in-comment", false, "Place move tags in optional first comment instead of caption")
		firstComment := fs.String("first-comment", "", "Custom optional first comment")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil {
			return ExitUsage
		}
		var ePtr, pPtr, aPtr, cPtr *string
		if *entryID != "" {
			ePtr = entryID
		}
		if *pubRevID != "" {
			pPtr = pubRevID
		}
		if *assetID != "" {
			aPtr = assetID
		}
		if *firstComment != "" {
			cPtr = firstComment
		}
		job, err := stack.pubSvc.CreateDraft(ctx, publishing.DraftRequest{
			AdminUser:            *stack.admin,
			PublicRevisionID:     pPtr,
			PrivateEntryID:       ePtr,
			AssetID:              aPtr,
			DestinationAccount:   *account,
			BrandPrefix:          *brand,
			IncludeCompanionTags: *companion,
			TagsInFirstComment:   *tagsInComment,
			CustomFirstComment:   cPtr,
		})
		if err != nil {
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(job)
		}
		a.printPublicationSummary(*job)
		return ExitOK

	case "preview", "status":
		fs := flag.NewFlagSet("publish "+sub, flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || fs.NArg() < 1 {
			fmt.Fprintf(a.Stderr, "usage: vueltario publish %s [--json] <publication_id>\n", sub)
			return ExitUsage
		}
		job, err := stack.store.GetPublicationJob(ctx, fs.Arg(0))
		if err != nil {
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(job)
		}
		a.printPublicationSummary(*job)
		return ExitOK

	case "approve":
		fs := flag.NewFlagSet("publish approve", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		hash := fs.String("hash", "", "Exact SHA-256 draft_hash to approve (required for noninteractive safety)")
		account := fs.String("account", "", "Confirm destination Instagram account")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || fs.NArg() < 1 || *hash == "" {
			fmt.Fprintln(a.Stderr, "usage: vueltario publish approve --hash <exact_draft_hash> [--account @cubansocial.sd] [--json] <publication_id>")
			return ExitUsage
		}
		job, err := stack.pubSvc.Approve(ctx, *stack.admin, fs.Arg(0), *hash, *account)
		if err != nil {
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(job)
		}
		fmt.Fprintf(a.Stdout, "Approved publication %s for %s (hash=%s)\n", job.ID, job.DestinationAccount, job.ApprovedDraftHash)
		return ExitOK

	case "run":
		fs := flag.NewFlagSet("publish run", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		dryRun := fs.Bool("dry-run", false, "Validate approval, rights, and media without creating a remote post")
		idemKey := fs.String("idempotency-key", "", "Idempotency key for execution")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || fs.NArg() < 1 {
			fmt.Fprintln(a.Stderr, "usage: vueltario publish run [--dry-run] [--idempotency-key ...] [--json] <publication_id>")
			return ExitUsage
		}
		job, err := stack.pubSvc.Execute(ctx, publishing.ExecuteOptions{
			AdminUser:      *stack.admin,
			PublicationID:  fs.Arg(0),
			IdempotencyKey: *idemKey,
			DryRun:         *dryRun,
		})
		if err != nil {
			if *asJSON && job != nil {
				_ = a.writeJSONOut(map[string]any{"error": err.Error(), "publication": job})
			} else {
				fmt.Fprintf(a.Stderr, "publish error: %v\n", err)
			}
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(job)
		}
		a.printPublicationSummary(*job)
		return ExitOK

	case "reconcile":
		fs := flag.NewFlagSet("publish reconcile", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		manualURL := fs.String("manual-url", "", "Confirmed Instagram URL for manual native repost handoff")
		retryComment := fs.Bool("retry-comment", false, "Retry failed optional first comment without republishing video")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || fs.NArg() < 1 {
			fmt.Fprintln(a.Stderr, "usage: vueltario publish reconcile [--manual-url ...] [--retry-comment] [--json] <publication_id>")
			return ExitUsage
		}
		job, err := stack.pubSvc.Reconcile(ctx, *stack.admin, fs.Arg(0), *manualURL, *retryComment)
		if err != nil {
			return a.mapError(err)
		}
		if *asJSON {
			return a.writeJSONOut(job)
		}
		a.printPublicationSummary(*job)
		return ExitOK

	default:
		fmt.Fprintf(a.Stderr, "unknown publish subcommand %q\n", sub)
		return ExitUsage
	}
}

func (a *App) printPublicationSummary(job domain.PublicationJob) {
	fmt.Fprintf(a.Stdout, "Publication ID: %s\n", job.ID)
	fmt.Fprintf(a.Stdout, "Destination:    %s (%s, origin=%s)\n", job.DestinationAccount, job.PublicationMode, job.OriginPlatform)
	fmt.Fprintf(a.Stdout, "Status:         %s — %s\n", job.Status, job.StatusDetail)
	fmt.Fprintf(a.Stdout, "Draft SHA-256:  %s\n", job.DraftHash)
	if job.ApprovedDraftHash != "" {
		fmt.Fprintf(a.Stdout, "Approved Hash:  %s\n", job.ApprovedDraftHash)
	}
	if job.ResultURL != "" {
		fmt.Fprintf(a.Stdout, "Result URL:     %s\n", job.ResultURL)
	}
	fmt.Fprintf(a.Stdout, "\n--- Caption Preview ---\n%s\n-----------------------\n", job.Caption)
	if job.FirstComment != "" {
		fmt.Fprintf(a.Stdout, "First Comment (%s): %s\n", job.FirstCommentStatus, job.FirstComment)
	}
	if job.NativeRepostGuide != "" {
		fmt.Fprintf(a.Stdout, "\n%s\n", job.NativeRepostGuide)
	}
}

func (a *App) writeJSONOut(v any) int {
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(a.Stderr, "json encode error: %v\n", err)
		return ExitError
	}
	return ExitOK
}

func (a *App) mapError(err error) int {
	fmt.Fprintf(a.Stderr, "error: %v\n", err)
	switch {
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrUnsupportedHost):
		return ExitUsage
	case errors.Is(err, domain.ErrUnauthorized), errors.Is(err, domain.ErrForbidden), errors.Is(err, publishing.ErrTokenExpired):
		return ExitBlocked
	case errors.Is(err, domain.ErrConflict), errors.Is(err, publishing.ErrApprovalInvalid), errors.Is(err, publishing.ErrAmbiguousOutcome):
		return ExitConflict
	default:
		return ExitError
	}
}
