package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds operational settings for Vueltario API, worker, and CLI.
type Config struct {
	Addr         string
	BaseURL      string
	CookieSecure bool

	DBDriver    string // "postgres" or "sqlite"
	DatabaseURL string

	AdminEmail    string
	AdminPassword string
	SignupMode    string // "invited_moderated", "open_moderated", "admin_only"

	StorageRoot            string
	MaxUploadBytes         int64
	MaxClipDurationMs      int64
	MaxConcurrentTranscode int
	AdminStorageQuotaBytes int64
	StagingTTL             time.Duration
	AIIntermediateTTL      time.Duration
	OrphanQuarantineTTL    time.Duration

	AnalysisEnabled          bool
	AnalysisDailyBudget      int
	AnalysisHighConfVerified bool

	YouTubeAPIKey string

	IGDestination        string
	IGBrandPrefix        string
	IGCaptionMaxChars    int
	IGMaxHashtags        int
	IGMaxMentions        int
	IGAllowCompanionTags bool
	IGAccessToken        string
	IGUserID             string
	IGAPIBaseURL         string

	FFprobeBinary string
	FFmpegBinary  string
}

// Load reads configuration from environment variables with PRD-aligned defaults.
func Load() Config {
	driver := strings.ToLower(envOr("VUELTARIO_DB_DRIVER", ""))
	dbURL := envOr("VUELTARIO_DATABASE_URL", "")
	if driver == "" {
		if strings.HasPrefix(dbURL, "postgres://") || strings.HasPrefix(dbURL, "postgresql://") {
			driver = "postgres"
		} else {
			driver = "sqlite"
		}
	}
	if dbURL == "" {
		if driver == "postgres" {
			dbURL = "postgres://vueltario:vueltario_dev_password@localhost:5432/vueltario?sslmode=disable"
		} else {
			dbURL = filepath.Join("data", "vueltario.db")
		}
	}

	baseURL := strings.TrimRight(envOr("VUELTARIO_BASE_URL", "http://localhost:8080"), "/")
	cookieSecure := envBool("VUELTARIO_COOKIE_SECURE", strings.HasPrefix(baseURL, "https://"))

	return Config{
		Addr:                     envOr("VUELTARIO_ADDR", ":8080"),
		BaseURL:                  baseURL,
		CookieSecure:             cookieSecure,
		DBDriver:                 driver,
		DatabaseURL:              dbURL,
		AdminEmail:               envOr("VUELTARIO_ADMIN_EMAIL", "admin@vueltario.com"),
		AdminPassword:            envOr("VUELTARIO_ADMIN_PASSWORD", "vueltario-local-admin"),
		SignupMode:               envOr("VUELTARIO_SIGNUP_MODE", "invited_moderated"),
		StorageRoot:              envOr("VUELTARIO_STORAGE_ROOT", filepath.Join("data", "storage")),
		MaxUploadBytes:           envInt64("VUELTARIO_MAX_UPLOAD_BYTES", 500*1024*1024),        // 500 MiB
		MaxClipDurationMs:        envInt64("VUELTARIO_MAX_CLIP_DURATION_MS", 10*60*1000),      // 10 minutes
		MaxConcurrentTranscode:   envInt("VUELTARIO_MAX_CONCURRENT_TRANSCODES", 2),            // 2 concurrent transcodes
		AdminStorageQuotaBytes:   envInt64("VUELTARIO_ADMIN_STORAGE_QUOTA_BYTES", 10<<30),     // 10 GiB
		StagingTTL:               24 * time.Hour,                                              // 24h post terminal job
		AIIntermediateTTL:        7 * 24 * time.Hour,                                          // 7 days max
		OrphanQuarantineTTL:      7 * 24 * time.Hour,                                          // 7 days quarantine
		AnalysisEnabled:          envBool("VUELTARIO_ANALYSIS_ENABLED", true),
		AnalysisDailyBudget:      envInt("VUELTARIO_ANALYSIS_DAILY_BUDGET", 200),
		AnalysisHighConfVerified: envBool("VUELTARIO_ANALYSIS_HIGH_CONF_VERIFIED", false),
		YouTubeAPIKey:            os.Getenv("VUELTARIO_YOUTUBE_API_KEY"),
		IGDestination:            envOr("VUELTARIO_IG_DESTINATION", "@cubansocial.sd"),
		IGBrandPrefix:            envOr("VUELTARIO_IG_BRAND_PREFIX", "CubanSocial"),
		IGCaptionMaxChars:        envInt("VUELTARIO_IG_CAPTION_MAX_CHARS", 2200),
		IGMaxHashtags:            envInt("VUELTARIO_IG_MAX_HASHTAGS", 30),
		IGMaxMentions:            envInt("VUELTARIO_IG_MAX_MENTIONS", 20),
		IGAllowCompanionTags:     envBool("VUELTARIO_IG_ALLOW_COMPANION_TAGS", true),
		IGAccessToken:            os.Getenv("VUELTARIO_IG_ACCESS_TOKEN"),
		IGUserID:                 os.Getenv("VUELTARIO_IG_USER_ID"),
		IGAPIBaseURL:             strings.TrimRight(envOr("VUELTARIO_IG_API_BASE_URL", "https://graph.instagram.com/v21.0"), "/"),
		FFprobeBinary:            envOr("VUELTARIO_FFPROBE_BIN", "ffprobe"),
		FFmpegBinary:             envOr("VUELTARIO_FFMPEG_BIN", "ffmpeg"),
	}
}

func (c Config) Validate() error {
	if c.DBDriver != "postgres" && c.DBDriver != "sqlite" {
		return fmt.Errorf("unsupported VUELTARIO_DB_DRIVER %q (expected postgres or sqlite)", c.DBDriver)
	}
	switch c.SignupMode {
	case "invited_moderated", "open_moderated", "admin_only":
	default:
		return fmt.Errorf("unsupported VUELTARIO_SIGNUP_MODE %q", c.SignupMode)
	}
	if c.MaxUploadBytes <= 0 {
		return fmt.Errorf("VUELTARIO_MAX_UPLOAD_BYTES must be positive")
	}
	if c.MaxConcurrentTranscode <= 0 {
		return fmt.Errorf("VUELTARIO_MAX_CONCURRENT_TRANSCODES must be >= 1")
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envInt64(key string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
