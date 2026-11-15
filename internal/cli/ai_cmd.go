package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/johandry/vueltario/internal/aiassist"
)

// cmdAI implements Option 2: CLI & local agent metadata/vuelta extraction (`vueltario ai <prompt|apply|frames|run-agent>`).
// Works against both the local SQLite/Postgres store and directly against remote Supabase (`--supabase`).
func (a *App) cmdAI(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Stderr, "usage: vueltario ai <prompt|apply|frames|run-agent> [flags]")
		return ExitUsage
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "prompt":
		fs := flag.NewFlagSet("ai prompt", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		entryRef := fs.String("entry", "", "Entry ID or video URL")
		useSupabase := fs.Bool("supabase", false, "Read entry & taxonomy directly from remote Supabase REST API")
		extraNote := fs.String("context", "", "Optional pasted caption/comments to embed in the prompt")
		videoFile := fs.String("video", "", "Optional local video file to sample JPEG frames from via ffmpeg")
		framesDir := fs.String("frames-dir", "./data/ai-frames", "Directory to write sampled video frames")
		interval := fs.Int("interval", 4, "Seconds between sampled frames when --video is provided")
		if err := fs.Parse(rest); err != nil || *entryRef == "" {
			fmt.Fprintln(a.Stderr, "usage: vueltario ai prompt --entry <id_or_url> [--supabase] [--context ...] [--video clip.mp4]")
			return ExitUsage
		}

		var framePaths []string
		if *videoFile != "" {
			sampled, err := aiassist.SampleVideoFrames(ctx, a.Cfg.FFmpegBinary, *videoFile, *framesDir, *interval, 8)
			if err != nil {
				fmt.Fprintf(a.Stderr, "frame sampling error: %v\n", err)
				return ExitError
			}
			framePaths = sampled
		}

		srcInfo, tax, code := a.resolveAIPromptInputs(ctx, *entryRef, *useSupabase)
		if code != ExitOK {
			return code
		}
		srcInfo.ExtraContext = *extraNote
		srcInfo.FramePaths = framePaths

		fmt.Fprintln(a.Stdout, aiassist.BuildExtractionPrompt(srcInfo, tax))
		return ExitOK

	case "apply":
		fs := flag.NewFlagSet("ai apply", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		entryRef := fs.String("entry", "", "Entry ID or video URL")
		inFile := fs.String("in", "", "Path to AI JSON file (reads stdin if omitted or '-')")
		useSupabase := fs.Bool("supabase", false, "Apply directly to remote Supabase REST API")
		replaceOccs := fs.Bool("replace-occurrences", false, "Replace existing occurrences instead of appending")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || *entryRef == "" {
			fmt.Fprintln(a.Stderr, "usage: vueltario ai apply --entry <id_or_url> [--in response.json] [--supabase] [--replace-occurrences] [--json]")
			return ExitUsage
		}

		var rawBytes []byte
		var err error
		if *inFile == "" || *inFile == "-" {
			rawBytes, err = io.ReadAll(os.Stdin)
		} else {
			rawBytes, err = os.ReadFile(*inFile)
		}
		if err != nil {
			fmt.Fprintf(a.Stderr, "read AI JSON failed: %v\n", err)
			return ExitUsage
		}

		return a.applyAIRawOutput(ctx, *entryRef, string(rawBytes), *useSupabase, *replaceOccs, *asJSON)

	case "frames":
		fs := flag.NewFlagSet("ai frames", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		videoFile := fs.String("video", "", "Path to local video file")
		outDir := fs.String("out", "./data/ai-frames", "Output directory for sampled JPEG frames")
		interval := fs.Int("interval", 4, "Seconds between sampled frames")
		maxFrames := fs.Int("max", 8, "Maximum frames to extract (1..24)")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || *videoFile == "" {
			fmt.Fprintln(a.Stderr, "usage: vueltario ai frames --video <clip.mp4> [--out ./frames] [--interval 4] [--max 8]")
			return ExitUsage
		}
		paths, err := aiassist.SampleVideoFrames(ctx, a.Cfg.FFmpegBinary, *videoFile, *outDir, *interval, *maxFrames)
		if err != nil {
			fmt.Fprintf(a.Stderr, "error: %v\n", err)
			return ExitError
		}
		if *asJSON {
			return a.writeJSONOut(map[string]any{"frames": paths})
		}
		for _, p := range paths {
			fmt.Fprintln(a.Stdout, p)
		}
		return ExitOK

	case "run-agent":
		fs := flag.NewFlagSet("ai run-agent", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		entryRef := fs.String("entry", "", "Entry ID or video URL")
		useSupabase := fs.Bool("supabase", false, "Read & write directly against remote Supabase REST API")
		agentBin := fs.String("bin", "", "Local AI CLI binary (e.g. ollama, llm, claude, gh)")
		agentArgs := fs.String("args", "", "Space-separated arguments passed to --bin (prompt is piped on stdin)")
		extraNote := fs.String("context", "", "Optional pasted caption/comments")
		videoFile := fs.String("video", "", "Optional local video file to sample frames from first")
		framesDir := fs.String("frames-dir", "./data/ai-frames", "Directory for sampled video frames")
		replaceOccs := fs.Bool("replace-occurrences", false, "Replace existing occurrences")
		asJSON := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(rest); err != nil || *entryRef == "" || *agentBin == "" {
			fmt.Fprintln(a.Stderr, "usage: vueltario ai run-agent --entry <id_or_url> --bin <agent_cli> [--args \"...\"] [--supabase] [--video clip.mp4]")
			return ExitUsage
		}

		var framePaths []string
		if *videoFile != "" {
			sampled, err := aiassist.SampleVideoFrames(ctx, a.Cfg.FFmpegBinary, *videoFile, *framesDir, 4, 8)
			if err != nil {
				fmt.Fprintf(a.Stderr, "frame sampling error: %v\n", err)
				return ExitError
			}
			framePaths = sampled
		}

		srcInfo, tax, code := a.resolveAIPromptInputs(ctx, *entryRef, *useSupabase)
		if code != ExitOK {
			return code
		}
		srcInfo.ExtraContext = *extraNote
		srcInfo.FramePaths = framePaths
		prompt := aiassist.BuildExtractionPrompt(srcInfo, tax)

		parsedArgs := strings.Fields(*agentArgs)
		agentOut, err := aiassist.RunLocalAgentCommand(ctx, prompt, *agentBin, parsedArgs)
		if err != nil {
			fmt.Fprintf(a.Stderr, "error: %v\n", err)
			return ExitError
		}
		return a.applyAIRawOutput(ctx, *entryRef, agentOut, *useSupabase, *replaceOccs, *asJSON)

	default:
		fmt.Fprintf(a.Stderr, "unknown ai subcommand %q\n", sub)
		return ExitUsage
	}
}

func (a *App) resolveAIPromptInputs(ctx context.Context, entryRef string, useSupabase bool) (aiassist.PromptSourceInfo, []aiassist.TaxonomyEntry, int) {
	if useSupabase {
		sb, err := aiassist.NewSupabaseRemote(a.Cfg)
		if err != nil {
			fmt.Fprintf(a.Stderr, "error: %v\n", err)
			return aiassist.PromptSourceInfo{}, nil, ExitUsage
		}
		srcInfo, tax, err := sb.LoadPromptContext(ctx, entryRef)
		if err != nil {
			return aiassist.PromptSourceInfo{}, nil, a.mapError(err)
		}
		return srcInfo, tax, ExitOK
	}

	stack, err := a.openStack(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		return aiassist.PromptSourceInfo{}, nil, ExitError
	}
	defer stack.store.Close()

	tax, err := aiassist.LoadTaxonomyFromStore(ctx, stack.store)
	if err != nil {
		return aiassist.PromptSourceInfo{}, nil, a.mapError(err)
	}
	ent, err := stack.store.GetEntry(ctx, stack.admin.ID, entryRef)
	if err != nil {
		return aiassist.PromptSourceInfo{}, nil, a.mapError(err)
	}
	title := ""
	if ent.PersonalTitleOverride != nil {
		title = *ent.PersonalTitleOverride
	} else if ent.Source.Title != nil {
		title = *ent.Source.Title
	}
	return aiassist.PromptSourceInfo{
		EntryID:      ent.ID,
		OriginalURL:  ent.Source.OriginalURL,
		CanonicalURL: ent.Source.CanonicalURL,
		Platform:     string(ent.Source.Platform),
		CurrentTitle: title,
	}, tax, ExitOK
}

func (a *App) applyAIRawOutput(ctx context.Context, entryRef, rawJSON string, useSupabase, replaceOccs, asJSON bool) int {
	if useSupabase {
		sb, err := aiassist.NewSupabaseRemote(a.Cfg)
		if err != nil {
			fmt.Fprintf(a.Stderr, "error: %v\n", err)
			return ExitUsage
		}
		srcInfo, tax, err := sb.LoadPromptContext(ctx, entryRef)
		if err != nil {
			return a.mapError(err)
		}
		parsed, err := aiassist.ParseAndValidateExtraction(rawJSON, tax)
		if err != nil {
			return a.mapError(err)
		}
		if err := sb.ApplyExtraction(ctx, srcInfo.EntryID, srcInfo.Platform, parsed, replaceOccs); err != nil {
			return a.mapError(err)
		}
		if asJSON {
			return a.writeJSONOut(parsed)
		}
		fmt.Fprintf(a.Stdout, "Applied AI metadata & %d vuelta(s) to Supabase entry %s\n", len(parsed.Occurrences), srcInfo.EntryID)
		return ExitOK
	}

	stack, err := a.openStack(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		return ExitError
	}
	defer stack.store.Close()

	tax, err := aiassist.LoadTaxonomyFromStore(ctx, stack.store)
	if err != nil {
		return a.mapError(err)
	}
	parsed, err := aiassist.ParseAndValidateExtraction(rawJSON, tax)
	if err != nil {
		return a.mapError(err)
	}
	updated, err := aiassist.ApplyToStore(ctx, stack.store, *stack.admin, entryRef, parsed, replaceOccs)
	if err != nil {
		return a.mapError(err)
	}
	if asJSON {
		return a.writeJSONOut(updated)
	}
	fmt.Fprintf(a.Stdout, "Applied AI metadata & %d vuelta(s) to entry %s (rev v%d)\n",
		len(parsed.Occurrences), updated.ID, updated.Revision)
	return ExitOK
}
