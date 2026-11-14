package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/netguard"
)

var (
	ErrCapabilityUnsupported = errors.New("provider capability unsupported")
	ErrProviderAuthRequired  = errors.New("provider authentication or API scope required")
	ErrRateLimitUpstream     = errors.New("upstream provider rate limited")
)

type CapabilityLevel string

const (
	CapVerified              CapabilityLevel = "verified"
	CapConditional           CapabilityLevel = "conditional"
	CapRequiresInvestigation CapabilityLevel = "requires_investigation"
	CapUnsupported           CapabilityLevel = "unsupported"
)

type ProviderCapabilities struct {
	Platform               domain.Platform `json:"platform"`
	PublicMetadata         CapabilityLevel `json:"public_metadata"`
	Comments               CapabilityLevel `json:"comments"`
	CaptionsAudio          CapabilityLevel `json:"captions_audio"`
	WebEmbeddedPlayback    CapabilityLevel `json:"web_embedded_playback"`
	InlineIOSPlayback      CapabilityLevel `json:"inline_ios_playback"`
	TimeAndSeek            CapabilityLevel `json:"time_and_seek"`
	ArbitraryLinkDownload  CapabilityLevel `json:"arbitrary_link_download"`
	AuthorizedFileImport   CapabilityLevel `json:"authorized_file_import"`
	PrecisionNote          string          `json:"precision_note"`
	IntegrationGateSummary string          `json:"integration_gate_summary"`
}

// PlaybackDescriptor is returned by GET /api/v1/sources/{id}/playback (never raw executable HTML).
type PlaybackDescriptor struct {
	SourceID                  string            `json:"source_id"`
	Platform                  domain.Platform   `json:"platform"`
	Mode                      string            `json:"mode"` // "authorized_asset", "iframe_embed", "fallback_only"
	EmbedURL                  string            `json:"embed_url,omitempty"`
	AssetStreamURL            string            `json:"asset_stream_url,omitempty"`
	AssetMIMEType             string            `json:"asset_mime_type,omitempty"`
	AllowedPostMessageOrigins []string          `json:"allowed_post_message_origins,omitempty"`
	SupportsPlayerTime        bool              `json:"supports_player_time"`
	SupportsSeek              bool              `json:"supports_seek"`
	PrecisionNote             string            `json:"precision_note"`
	AvailabilityState         string            `json:"availability_state"`
	FallbackReason            string            `json:"fallback_reason,omitempty"`
	OpenOriginalURL           string            `json:"open_original_url"`
	Capabilities              ProviderCapabilities `json:"capabilities"`
}

type FetchedMetadata struct {
	Title             *string
	Description       *string
	DurationMs        *int64
	CreatorName       *string
	CreatorHandle     *string
	CreatorProfileURL *string
	CreatorPlatformID *string
	AvailabilityState domain.AvailabilityState
	EmbedEligible     bool
	Provenance        string
	RetryAfter        time.Duration
}

type FetchedComment struct {
	CommentID  string
	AuthorName string
	IsCreator  bool
	Text       string
	Published  time.Time
}

type Adapter interface {
	Platform() domain.Platform
	Capabilities() ProviderCapabilities
	BuildEmbedDescriptor(src domain.Source) PlaybackDescriptor
	FetchMetadata(ctx context.Context, src domain.Source) (FetchedMetadata, error)
	FetchComments(ctx context.Context, src domain.Source, maxItems int) ([]FetchedComment, error)
}

type Registry struct {
	adapters map[domain.Platform]Adapter
}

func NewDefaultRegistry(httpClient *netguard.SafeClient, youtubeAPIKey string) *Registry {
	if httpClient == nil {
		httpClient = netguard.NewSafeClient(netguard.Options{})
	}
	r := &Registry{adapters: make(map[domain.Platform]Adapter)}
	r.Register(&YouTubeAdapter{client: httpClient, apiKey: youtubeAPIKey})
	r.Register(&TikTokAdapter{client: httpClient})
	r.Register(&FacebookAdapter{client: httpClient})
	r.Register(&InstagramAdapter{client: httpClient})
	return r
}

func (r *Registry) Register(a Adapter) {
	r.adapters[a.Platform()] = a
}

func (r *Registry) Get(p domain.Platform) (Adapter, bool) {
	a, ok := r.adapters[p]
	return a, ok
}

func (r *Registry) AllCapabilities() []ProviderCapabilities {
	order := []domain.Platform{
		domain.PlatformYouTube,
		domain.PlatformTikTok,
		domain.PlatformFacebook,
		domain.PlatformInstagram,
	}
	out := make([]ProviderCapabilities, 0, len(order))
	for _, p := range order {
		if a, ok := r.adapters[p]; ok {
			out = append(out, a.Capabilities())
		}
	}
	return out
}

// DescribePlayback resolves the safest permitted playback descriptor for a source,
// preferring an authorized ready MediaAsset when the caller has access to one.
func (r *Registry) DescribePlayback(src domain.Source, permittedAsset *domain.MediaAsset) PlaybackDescriptor {
	adapter, ok := r.Get(src.Platform)
	var caps ProviderCapabilities
	if ok {
		caps = adapter.Capabilities()
	}
	if permittedAsset != nil && permittedAsset.Status == "ready" && permittedAsset.StreamURL != "" {
		return PlaybackDescriptor{
			SourceID:           src.ID,
			Platform:           src.Platform,
			Mode:               "authorized_asset",
			AssetStreamURL:     permittedAsset.StreamURL,
			AssetMIMEType:      permittedAsset.MIMEType,
			SupportsPlayerTime: true,
			SupportsSeek:       true,
			PrecisionNote:      "HTML5 video playback of authorized asset; timing accurate to browser media clock (~50ms), not guaranteed frame-exact.",
			AvailabilityState:  string(src.AvailabilityState),
			OpenOriginalURL:    src.OriginalURL,
			Capabilities:       caps,
		}
	}
	if !ok {
		return PlaybackDescriptor{
			SourceID:          src.ID,
			Platform:          src.Platform,
			Mode:              "fallback_only",
			AvailabilityState: string(src.AvailabilityState),
			FallbackReason:    "No provider adapter registered for this platform.",
			OpenOriginalURL:   src.OriginalURL,
		}
	}
	return adapter.BuildEmbedDescriptor(src)
}

// --- YouTube Adapter ---

type YouTubeAdapter struct {
	client *netguard.SafeClient
	apiKey string
}

func (a *YouTubeAdapter) Platform() domain.Platform { return domain.PlatformYouTube }

func (a *YouTubeAdapter) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Platform:               domain.PlatformYouTube,
		PublicMetadata:         CapVerified,
		Comments:               CapVerified,
		CaptionsAudio:          CapConditional,
		WebEmbeddedPlayback:    CapVerified,
		InlineIOSPlayback:      CapConditional,
		TimeAndSeek:            CapVerified,
		ArbitraryLinkDownload:  CapUnsupported,
		AuthorizedFileImport:   CapVerified,
		PrecisionNote:          "YouTube IFrame Player API exposes currentTime and seekTo (approximate to keyframe/stream buffer, not frame-exact).",
		IntegrationGateSummary: "Data API v3 used when VUELTARIO_YOUTUBE_API_KEY is configured; falls back to public oEmbed. Captions download requires video edit permission.",
	}
}

func (a *YouTubeAdapter) BuildEmbedDescriptor(src domain.Source) PlaybackDescriptor {
	caps := a.Capabilities()
	desc := PlaybackDescriptor{
		SourceID:          src.ID,
		Platform:          domain.PlatformYouTube,
		AvailabilityState: string(src.AvailabilityState),
		OpenOriginalURL:   src.OriginalURL,
		Capabilities:      caps,
		PrecisionNote:     caps.PrecisionNote,
	}
	if !src.EmbedEligible || src.AvailabilityState == domain.AvailabilityPrivate ||
		src.AvailabilityState == domain.AvailabilityDeleted || src.AvailabilityState == domain.AvailabilityLoginRequired ||
		src.AvailabilityState == domain.AvailabilityRestricted {
		desc.Mode = "fallback_only"
		desc.FallbackReason = explainUnavailable(src.AvailabilityState, "YouTube video disables external embedding or is unavailable.")
		return desc
	}
	if src.PlatformContentID == nil || *src.PlatformContentID == "" {
		desc.Mode = "fallback_only"
		desc.FallbackReason = "Stable YouTube video ID could not be extracted from this URL; use Open original or enter timestamps manually."
		return desc
	}
	embedURL := fmt.Sprintf("https://www.youtube-nocookie.com/embed/%s?enablejsapi=1&rel=0&playsinline=1", url.PathEscape(*src.PlatformContentID))
	desc.Mode = "iframe_embed"
	desc.EmbedURL = embedURL
	desc.AllowedPostMessageOrigins = []string{"https://www.youtube-nocookie.com", "https://www.youtube.com"}
	desc.SupportsPlayerTime = true
	desc.SupportsSeek = true
	return desc
}

func (a *YouTubeAdapter) FetchMetadata(ctx context.Context, src domain.Source) (FetchedMetadata, error) {
	if a.apiKey != "" && src.PlatformContentID != nil && *src.PlatformContentID != "" {
		return a.fetchDataAPI(ctx, *src.PlatformContentID)
	}
	// Permitted public oEmbed fallback
	endpoint := "https://www.youtube.com/oembed?format=json&url=" + url.QueryEscape(src.CanonicalURL)
	body, status, headers, err := a.client.GetBytes(ctx, endpoint, nil)
	if err != nil {
		return FetchedMetadata{}, err
	}
	if status == 401 || status == 403 {
		return FetchedMetadata{AvailabilityState: domain.AvailabilityRestricted, EmbedEligible: false, Provenance: "youtube_oembed"}, nil
	}
	if status == 404 {
		return FetchedMetadata{AvailabilityState: domain.AvailabilityDeleted, EmbedEligible: false, Provenance: "youtube_oembed"}, nil
	}
	if status == 429 {
		return FetchedMetadata{RetryAfter: parseRetryAfter(headers.Get("Retry-After"))}, ErrRateLimitUpstream
	}
	if status != 200 {
		return FetchedMetadata{}, fmt.Errorf("youtube oembed HTTP %d", status)
	}
	var payload struct {
		Title      string `json:"title"`
		AuthorName string `json:"author_name"`
		AuthorURL  string `json:"author_url"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return FetchedMetadata{}, err
	}
	out := FetchedMetadata{
		AvailabilityState: domain.AvailabilityAvailable,
		EmbedEligible:     true,
		Provenance:        "youtube_oembed",
	}
	if strings.TrimSpace(payload.Title) != "" {
		t := strings.TrimSpace(payload.Title)
		out.Title = &t
	}
	if strings.TrimSpace(payload.AuthorName) != "" {
		c := strings.TrimSpace(payload.AuthorName)
		out.CreatorName = &c
	}
	if strings.TrimSpace(payload.AuthorURL) != "" {
		u := strings.TrimSpace(payload.AuthorURL)
		out.CreatorProfileURL = &u
		if idx := strings.Index(u, "/@"); idx >= 0 {
			h := strings.Trim(u[idx+2:], "/")
			out.CreatorHandle = &h
		}
	}
	return out, nil
}

func (a *YouTubeAdapter) fetchDataAPI(ctx context.Context, videoID string) (FetchedMetadata, error) {
	q := url.Values{}
	q.Set("part", "snippet,contentDetails,status")
	q.Set("id", videoID)
	q.Set("key", a.apiKey)
	endpoint := "https://www.googleapis.com/youtube/v3/videos?" + q.Encode()
	body, status, headers, err := a.client.GetBytes(ctx, endpoint, nil)
	if err != nil {
		return FetchedMetadata{}, err
	}
	if status == 429 {
		return FetchedMetadata{RetryAfter: parseRetryAfter(headers.Get("Retry-After"))}, ErrRateLimitUpstream
	}
	if status == 401 || status == 403 {
		return FetchedMetadata{}, ErrProviderAuthRequired
	}
	if status != 200 {
		return FetchedMetadata{}, fmt.Errorf("youtube data api HTTP %d", status)
	}
	var resp struct {
		Items []struct {
			Snippet struct {
				Title        string `json:"title"`
				Description  string `json:"description"`
				ChannelID    string `json:"channelId"`
				ChannelTitle string `json:"channelTitle"`
			} `json:"snippet"`
			ContentDetails struct {
				Duration string `json:"duration"`
			} `json:"contentDetails"`
			Status struct {
				Embeddable    bool   `json:"embeddable"`
				PrivacyStatus string `json:"privacyStatus"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return FetchedMetadata{}, err
	}
	if len(resp.Items) == 0 {
		return FetchedMetadata{AvailabilityState: domain.AvailabilityDeleted, EmbedEligible: false, Provenance: "youtube_data_api_v3"}, nil
	}
	item := resp.Items[0]
	avail := domain.AvailabilityAvailable
	if item.Status.PrivacyStatus == "private" {
		avail = domain.AvailabilityPrivate
	}
	title := strings.TrimSpace(item.Snippet.Title)
	desc := strings.TrimSpace(item.Snippet.Description)
	chTitle := strings.TrimSpace(item.Snippet.ChannelTitle)
	chID := strings.TrimSpace(item.Snippet.ChannelID)
	chURL := "https://www.youtube.com/channel/" + chID
	out := FetchedMetadata{
		Title:             nonEmptyPtr(title),
		Description:       nonEmptyPtr(desc),
		DurationMs:        parseISO8601DurationMs(item.ContentDetails.Duration),
		CreatorName:       nonEmptyPtr(chTitle),
		CreatorPlatformID: nonEmptyPtr(chID),
		CreatorProfileURL: nonEmptyPtr(chURL),
		AvailabilityState: avail,
		EmbedEligible:     item.Status.Embeddable,
		Provenance:        "youtube_data_api_v3",
	}
	return out, nil
}

func (a *YouTubeAdapter) FetchComments(ctx context.Context, src domain.Source, maxItems int) ([]FetchedComment, error) {
	if a.apiKey == "" {
		return nil, ErrProviderAuthRequired
	}
	if src.PlatformContentID == nil || *src.PlatformContentID == "" {
		return nil, ErrCapabilityUnsupported
	}
	if maxItems <= 0 || maxItems > 25 {
		maxItems = 15
	}
	q := url.Values{}
	q.Set("part", "snippet")
	q.Set("videoId", *src.PlatformContentID)
	q.Set("maxResults", strconv.Itoa(maxItems))
	q.Set("textFormat", "plainText")
	q.Set("order", "relevance")
	q.Set("key", a.apiKey)
	endpoint := "https://www.googleapis.com/youtube/v3/commentThreads?" + q.Encode()
	body, status, _, err := a.client.GetBytes(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if status == 403 {
		return nil, ErrCapabilityUnsupported // Comments disabled or restricted
	}
	if status != 200 {
		return nil, fmt.Errorf("youtube comments HTTP %d", status)
	}
	var resp struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				TopLevelComment struct {
					Snippet struct {
						AuthorDisplayName string    `json:"authorDisplayName"`
						TextDisplay       string    `json:"textDisplay"`
						PublishedAt       time.Time `json:"publishedAt"`
					} `json:"snippet"`
				} `json:"topLevelComment"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	var comments []FetchedComment
	for _, item := range resp.Items {
		s := item.Snippet.TopLevelComment.Snippet
		excerpt := truncateRunes(strings.TrimSpace(s.TextDisplay), 400)
		if excerpt == "" {
			continue
		}
		comments = append(comments, FetchedComment{
			CommentID:  item.ID,
			AuthorName: s.AuthorDisplayName,
			Text:       excerpt,
			Published:  s.PublishedAt,
		})
	}
	return comments, nil
}

// --- TikTok Adapter ---

type TikTokAdapter struct {
	client *netguard.SafeClient
}

func (a *TikTokAdapter) Platform() domain.Platform { return domain.PlatformTikTok }

func (a *TikTokAdapter) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Platform:               domain.PlatformTikTok,
		PublicMetadata:         CapConditional,
		Comments:               CapRequiresInvestigation,
		CaptionsAudio:          CapUnsupported,
		WebEmbeddedPlayback:    CapVerified,
		InlineIOSPlayback:      CapConditional,
		TimeAndSeek:            CapVerified,
		ArbitraryLinkDownload:  CapUnsupported,
		AuthorizedFileImport:   CapVerified,
		PrecisionNote:          "TikTok Embed Player v1 supports postMessage onCurrentTime and seekTo when video creator allows embedding (approximate second/sub-second precision).",
		IntegrationGateSummary: "Public oEmbed used for accessible videos; comments and audio extraction are not assumed for consumer apps.",
	}
}

func (a *TikTokAdapter) BuildEmbedDescriptor(src domain.Source) PlaybackDescriptor {
	caps := a.Capabilities()
	desc := PlaybackDescriptor{
		SourceID:          src.ID,
		Platform:          domain.PlatformTikTok,
		AvailabilityState: string(src.AvailabilityState),
		OpenOriginalURL:   src.OriginalURL,
		Capabilities:      caps,
		PrecisionNote:     caps.PrecisionNote,
	}
	if !src.EmbedEligible || src.AvailabilityState == domain.AvailabilityPrivate ||
		src.AvailabilityState == domain.AvailabilityDeleted || src.AvailabilityState == domain.AvailabilityLoginRequired ||
		src.AvailabilityState == domain.AvailabilityRestricted {
		desc.Mode = "fallback_only"
		desc.FallbackReason = explainUnavailable(src.AvailabilityState, "TikTok creator disabled embedding or post is restricted.")
		return desc
	}
	if src.PlatformContentID == nil || *src.PlatformContentID == "" {
		desc.Mode = "fallback_only"
		desc.FallbackReason = "Short TikTok link does not expose a numeric video ID for the Embed Player; enter timestamps manually or open the original post."
		return desc
	}
	desc.Mode = "iframe_embed"
	desc.EmbedURL = fmt.Sprintf("https://www.tiktok.com/player/v1/%s?music_info=1&description=1", url.PathEscape(*src.PlatformContentID))
	desc.AllowedPostMessageOrigins = []string{"https://www.tiktok.com"}
	desc.SupportsPlayerTime = true
	desc.SupportsSeek = true
	return desc
}

func (a *TikTokAdapter) FetchMetadata(ctx context.Context, src domain.Source) (FetchedMetadata, error) {
	endpoint := "https://www.tiktok.com/oembed?url=" + url.QueryEscape(src.CanonicalURL)
	body, status, headers, err := a.client.GetBytes(ctx, endpoint, nil)
	if err != nil {
		return FetchedMetadata{}, err
	}
	if status == 429 {
		return FetchedMetadata{RetryAfter: parseRetryAfter(headers.Get("Retry-After"))}, ErrRateLimitUpstream
	}
	if status == 400 || status == 404 {
		return FetchedMetadata{AvailabilityState: domain.AvailabilityRestricted, EmbedEligible: false, Provenance: "tiktok_oembed"}, nil
	}
	if status != 200 {
		return FetchedMetadata{}, fmt.Errorf("tiktok oembed HTTP %d", status)
	}
	var payload struct {
		Title        string `json:"title"`
		AuthorName   string `json:"author_name"`
		AuthorURL    string `json:"author_url"`
		AuthorUnique string `json:"author_unique_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return FetchedMetadata{}, err
	}
	handle := strings.TrimPrefix(strings.TrimSpace(payload.AuthorUnique), "@")
	if handle == "" && strings.Contains(payload.AuthorURL, "/@") {
		idx := strings.Index(payload.AuthorURL, "/@")
		handle = strings.Trim(payload.AuthorURL[idx+2:], "/")
	}
	return FetchedMetadata{
		Title:             nonEmptyPtr(strings.TrimSpace(payload.Title)),
		Description:       nonEmptyPtr(strings.TrimSpace(payload.Title)),
		CreatorName:       nonEmptyPtr(strings.TrimSpace(payload.AuthorName)),
		CreatorHandle:     nonEmptyPtr(handle),
		CreatorProfileURL: nonEmptyPtr(strings.TrimSpace(payload.AuthorURL)),
		AvailabilityState: domain.AvailabilityAvailable,
		EmbedEligible:     true,
		Provenance:        "tiktok_oembed",
	}, nil
}

func (a *TikTokAdapter) FetchComments(ctx context.Context, src domain.Source, maxItems int) ([]FetchedComment, error) {
	return nil, ErrCapabilityUnsupported
}

// --- Facebook Adapter ---

type FacebookAdapter struct {
	client *netguard.SafeClient
}

func (a *FacebookAdapter) Platform() domain.Platform { return domain.PlatformFacebook }

func (a *FacebookAdapter) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Platform:               domain.PlatformFacebook,
		PublicMetadata:         CapConditional,
		Comments:               CapConditional,
		CaptionsAudio:          CapRequiresInvestigation,
		WebEmbeddedPlayback:    CapVerified,
		InlineIOSPlayback:      CapConditional,
		TimeAndSeek:            CapRequiresInvestigation,
		ArbitraryLinkDownload:  CapRequiresInvestigation,
		AuthorizedFileImport:   CapVerified,
		PrecisionNote:          "Public Facebook videos support the official Embedded Video Player plugin, but programmatic time/seek is unverified [S8]; numeric MM:SS.mmm entry is provided.",
		IntegrationGateSummary: "Meta oEmbed Read requires approved Meta App token; catalog entry and manual timing work without Meta API tokens.",
	}
}

func (a *FacebookAdapter) BuildEmbedDescriptor(src domain.Source) PlaybackDescriptor {
	caps := a.Capabilities()
	desc := PlaybackDescriptor{
		SourceID:          src.ID,
		Platform:          domain.PlatformFacebook,
		AvailabilityState: string(src.AvailabilityState),
		OpenOriginalURL:   src.OriginalURL,
		Capabilities:      caps,
		PrecisionNote:     caps.PrecisionNote,
	}
	if !src.EmbedEligible || src.AvailabilityState == domain.AvailabilityPrivate ||
		src.AvailabilityState == domain.AvailabilityDeleted || src.AvailabilityState == domain.AvailabilityLoginRequired ||
		src.AvailabilityState == domain.AvailabilityRestricted {
		desc.Mode = "fallback_only"
		desc.FallbackReason = explainUnavailable(src.AvailabilityState, "Facebook video requires login, restricts embedding, or is private.")
		return desc
	}
	desc.Mode = "iframe_embed"
	desc.EmbedURL = "https://www.facebook.com/plugins/video.php?show_text=false&href=" + url.QueryEscape(src.CanonicalURL)
	desc.AllowedPostMessageOrigins = []string{"https://www.facebook.com"}
	desc.SupportsPlayerTime = false // Honest capability: JS SDK timing unverified [S8]
	desc.SupportsSeek = false
	return desc
}

func (a *FacebookAdapter) FetchMetadata(ctx context.Context, src domain.Source) (FetchedMetadata, error) {
	// Arbitrary-post Graph/oEmbed requires approved Meta oEmbed Read scope [S7-S8]; never scrape HTML server-side (Section 13).
	return FetchedMetadata{}, ErrCapabilityUnsupported
}

func (a *FacebookAdapter) FetchComments(ctx context.Context, src domain.Source, maxItems int) ([]FetchedComment, error) {
	return nil, ErrCapabilityUnsupported
}

// --- Instagram Adapter ---

type InstagramAdapter struct {
	client *netguard.SafeClient
}

func (a *InstagramAdapter) Platform() domain.Platform { return domain.PlatformInstagram }

func (a *InstagramAdapter) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Platform:               domain.PlatformInstagram,
		PublicMetadata:         CapConditional,
		Comments:               CapConditional,
		CaptionsAudio:          CapRequiresInvestigation,
		WebEmbeddedPlayback:    CapConditional,
		InlineIOSPlayback:      CapRequiresInvestigation,
		TimeAndSeek:            CapUnsupported,
		ArbitraryLinkDownload:  CapRequiresInvestigation,
		AuthorizedFileImport:   CapVerified,
		PrecisionNote:          "Instagram embeds depend on post visibility/embed settings and do not expose a verified player currentTime/seek API; use numeric timestamp entry or import an authorized file.",
		IntegrationGateSummary: "Arbitrary Instagram post metadata requires approved Meta oEmbed scope [S9]. Publishing uses Instagram Login Content Publishing API or manual native-repost handoff [S11-S13].",
	}
}

func (a *InstagramAdapter) BuildEmbedDescriptor(src domain.Source) PlaybackDescriptor {
	caps := a.Capabilities()
	desc := PlaybackDescriptor{
		SourceID:          src.ID,
		Platform:          domain.PlatformInstagram,
		AvailabilityState: string(src.AvailabilityState),
		OpenOriginalURL:   src.OriginalURL,
		Capabilities:      caps,
		PrecisionNote:     caps.PrecisionNote,
	}
	if !src.EmbedEligible || src.AvailabilityState == domain.AvailabilityPrivate ||
		src.AvailabilityState == domain.AvailabilityDeleted || src.AvailabilityState == domain.AvailabilityLoginRequired ||
		src.AvailabilityState == domain.AvailabilityRestricted {
		desc.Mode = "fallback_only"
		desc.FallbackReason = explainUnavailable(src.AvailabilityState, "Instagram post playback is unavailable inline (private, login-required, or embedding disabled).")
		return desc
	}
	if src.PlatformContentID == nil || *src.PlatformContentID == "" {
		desc.Mode = "fallback_only"
		desc.FallbackReason = "Instagram post shortcode not recognized for inline embed; use Open original and numeric timestamp entry."
		return desc
	}
	kind := "reel"
	if strings.Contains(src.CanonicalURL, "/p/") {
		kind = "p"
	} else if strings.Contains(src.CanonicalURL, "/tv/") {
		kind = "tv"
	}
	desc.Mode = "iframe_embed"
	desc.EmbedURL = fmt.Sprintf("https://www.instagram.com/%s/%s/embed/", kind, url.PathEscape(*src.PlatformContentID))
	desc.AllowedPostMessageOrigins = []string{"https://www.instagram.com"}
	desc.SupportsPlayerTime = false
	desc.SupportsSeek = false
	return desc
}

func (a *InstagramAdapter) FetchMetadata(ctx context.Context, src domain.Source) (FetchedMetadata, error) {
	return FetchedMetadata{}, ErrCapabilityUnsupported
}

func (a *InstagramAdapter) FetchComments(ctx context.Context, src domain.Source, maxItems int) ([]FetchedComment, error) {
	return nil, ErrCapabilityUnsupported
}

// --- Phase 0 Representative Link Fixtures ---

type LinkFixture struct {
	Name              string          `json:"name"`
	URL               string          `json:"url"`
	ExpectedPlatform  domain.Platform `json:"expected_platform"`
	ExpectedContentID string          `json:"expected_content_id,omitempty"`
	ExpectedOffsetMs  *int64          `json:"expected_offset_ms,omitempty"`
	Scenario          string          `json:"scenario"`
}

func RepresentativeFixtures() []LinkFixture {
	off23 := int64(23000)
	off85 := int64(85000)
	return []LinkFixture{
		{
			Name:              "YouTube standard watch with timestamp & tracking",
			URL:               "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=23s&utm_source=share&si=abc123",
			ExpectedPlatform:  domain.PlatformYouTube,
			ExpectedContentID: "dQw4w9WgXcQ",
			ExpectedOffsetMs:  &off23,
			Scenario:          "Eligible YouTube IFrame API + time/seek",
		},
		{
			Name:              "YouTube short link with minute-second offset",
			URL:               "https://youtu.be/M7lc1UVf-VE?t=1m25s",
			ExpectedPlatform:  domain.PlatformYouTube,
			ExpectedContentID: "M7lc1UVf-VE",
			ExpectedOffsetMs:  &off85,
			Scenario:          "Short link canonicalization + offset preservation",
		},
		{
			Name:              "TikTok creator video URL",
			URL:               "https://www.tiktok.com/@cubansalsa_demo/video/7345678901234567890?_t=1710000000&_r=1",
			ExpectedPlatform:  domain.PlatformTikTok,
			ExpectedContentID: "7345678901234567890",
			Scenario:          "Eligible TikTok Embed Player v1 + tracking removal",
		},
		{
			Name:             "TikTok short share URL",
			URL:              "https://vm.tiktok.com/ZMMabc123/",
			ExpectedPlatform: domain.PlatformTikTok,
			Scenario:         "Short URL without numeric ID -> manual timing fallback",
		},
		{
			Name:              "Facebook Watch public video",
			URL:               "https://www.facebook.com/watch/?v=10153231379946729&fbclid=IwAR123",
			ExpectedPlatform:  domain.PlatformFacebook,
			ExpectedContentID: "10153231379946729",
			Scenario:          "Eligible Facebook Embedded Video plugin + numeric timing fallback",
		},
		{
			Name:              "Instagram Reel public URL",
			URL:               "https://www.instagram.com/reel/C8abcXYZ123/?igsh=MzRlODBiNWFlZA==",
			ExpectedPlatform:  domain.PlatformInstagram,
			ExpectedContentID: "C8abcXYZ123",
			Scenario:          "Conditional Instagram embed + numeric timing + native repost handoff source",
		},
	}
}

// --- Helpers ---

var isoDurationRe = regexp.MustCompile(`^PT(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?$`)

func parseISO8601DurationMs(iso string) *int64 {
	m := isoDurationRe.FindStringSubmatch(strings.TrimSpace(iso))
	if len(m) != 4 {
		return nil
	}
	hrs, _ := strconv.Atoi(m[1])
	mins, _ := strconv.Atoi(m[2])
	secs, _ := strconv.Atoi(m[3])
	ms := int64(hrs*3600+mins*60+secs) * 1000
	return &ms
}

func parseRetryAfter(val string) time.Duration {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0
	}
	if secs, err := strconv.Atoi(val); err == nil && secs > 0 {
		if secs > 300 {
			secs = 300
		}
		return time.Duration(secs) * time.Second
	}
	return 0
}

func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func explainUnavailable(state domain.AvailabilityState, fallback string) string {
	switch state {
	case domain.AvailabilityPrivate:
		return "This video is marked private on the source platform. Inline playback is unavailable; use Open original if you have permission."
	case domain.AvailabilityDeleted:
		return "This video appears to have been deleted or removed on the source platform."
	case domain.AvailabilityLoginRequired:
		return "The source platform requires signing in to view this post. Inline playback is unavailable; use Open original to view directly."
	case domain.AvailabilityRestricted:
		return "The creator or platform restricts inline embedding for this video. Use Open original and enter timestamps manually."
	default:
		return fallback
	}
}
