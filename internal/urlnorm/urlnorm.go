package urlnorm

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/johandry/vueltario/internal/domain"
)

// NormalizedLink holds the deterministic output of ING-01 and ING-02.
type NormalizedLink struct {
	OriginalURL       string          `json:"original_url"`
	CanonicalURL      string          `json:"canonical_url"`
	Platform          domain.Platform `json:"platform"`
	PlatformContentID *string         `json:"platform_content_id"`
	ObservedOffsetMs  *int64          `json:"observed_offset_ms"`
}

var knownTrackingParams = map[string]bool{
	"utm_source": true, "utm_medium": true, "utm_campaign": true, "utm_term": true, "utm_content": true,
	"utm_id": true, "fbclid": true, "gclid": true, "igsh": true, "igshid": true, "si": true,
	"_t": true, "_r": true, "is_from_webapp": true, "sender_device": true, "sender_web_id": true,
	"feature": true, "pp": true, "ab_channel": true, "rdid": true, "share_url": true,
	"mibext": true, "mibextid": true, "ref": true, "refsrc": true, "s": true,
}

var (
	ytIDPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	tiktokVideoID     = regexp.MustCompile(`^/(@[^/]+)/video/(\d+)`)
	tiktokEmbedOrV    = regexp.MustCompile(`^/(?:v|embed/v2)/(\d+)`)
	tiktokShortPath   = regexp.MustCompile(`^/(?:t/)?([A-Za-z0-9_-]{6,24})/?$`)
	igPostPattern     = regexp.MustCompile(`^/(?:[^/]+/)?(p|reel|reels|tv)/([A-Za-z0-9_-]+)`)
	fbVideosPattern   = regexp.MustCompile(`/(?:videos|reel)/(\d+)`)
	fbSharePattern    = regexp.MustCompile(`^/share/(?:v|r)/([A-Za-z0-9_-]+)`)
	ytDurationPattern = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s?)?$`)
	urlInTextPattern  = regexp.MustCompile(`https?://[^\s<>"]+`)
)

// Normalize validates, identifies the platform, extracts stable IDs and playback offsets,
// and strips tracking query parameters while keeping the caller's original URL intact.
func Normalize(rawInput string) (NormalizedLink, error) {
	trimmed := strings.TrimSpace(rawInput)
	res := NormalizedLink{OriginalURL: trimmed}
	if trimmed == "" {
		return res, fmt.Errorf("%w: URL is required", domain.ErrValidation)
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return res, fmt.Errorf("%w: invalid URL %q", domain.ErrValidation, trimmed)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return res, fmt.Errorf("%w: URL must use http or https", domain.ErrValidation)
	}

	host := strings.ToLower(parsed.Hostname())
	platform, ok := classifyHost(host)
	if !ok {
		return res, fmt.Errorf("%w: %q is not supported (supported platforms: YouTube, TikTok, Facebook, Instagram)", domain.ErrUnsupportedHost, host)
	}
	res.Platform = platform

	// Extract playback offset before normalizing query parameters.
	if offset := extractOffsetMs(parsed); offset != nil {
		res.ObservedOffsetMs = offset
	}

	switch platform {
	case domain.PlatformYouTube:
		normalizeYouTube(parsed, host, &res)
	case domain.PlatformTikTok:
		normalizeTikTok(parsed, host, &res)
	case domain.PlatformInstagram:
		normalizeInstagram(parsed, &res)
	case domain.PlatformFacebook:
		normalizeFacebook(parsed, host, &res)
	}

	if res.CanonicalURL == "" {
		res.CanonicalURL = fallbackCleanURL(parsed)
	}
	return res, nil
}

// ExtractSupportedURLsFromText extracts and normalizes all supported social video links from arbitrary shared text.
func ExtractSupportedURLsFromText(text string) []NormalizedLink {
	matches := urlInTextPattern.FindAllString(text, -1)
	seen := make(map[string]bool)
	var out []NormalizedLink
	for _, m := range matches {
		cleaned := strings.TrimRight(m, ".,;:!?)")
		norm, err := Normalize(cleaned)
		if err != nil {
			continue
		}
		key := norm.CanonicalURL
		if norm.PlatformContentID != nil && *norm.PlatformContentID != "" {
			key = string(norm.Platform) + ":" + *norm.PlatformContentID
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, norm)
		}
	}
	return out
}

func classifyHost(host string) (domain.Platform, bool) {
	switch {
	case host == "youtu.be" || host == "youtube.com" || strings.HasSuffix(host, ".youtube.com"):
		return domain.PlatformYouTube, true
	case host == "tiktok.com" || strings.HasSuffix(host, ".tiktok.com"):
		return domain.PlatformTikTok, true
	case host == "instagram.com" || strings.HasSuffix(host, ".instagram.com") || host == "instagr.am":
		return domain.PlatformInstagram, true
	case host == "facebook.com" || strings.HasSuffix(host, ".facebook.com") || host == "fb.watch" || host == "fb.com":
		return domain.PlatformFacebook, true
	default:
		return "", false
	}
}

func normalizeYouTube(u *url.URL, host string, out *NormalizedLink) {
	path := strings.TrimRight(u.Path, "/")
	var vid string
	if host == "youtu.be" {
		seg := strings.TrimPrefix(path, "/")
		if idx := strings.IndexByte(seg, '/'); idx >= 0 {
			seg = seg[:idx]
		}
		vid = seg
	} else if path == "/watch" {
		vid = u.Query().Get("v")
	} else {
		for _, prefix := range []string{"/shorts/", "/embed/", "/live/", "/v/"} {
			if strings.HasPrefix(path, prefix) {
				seg := strings.TrimPrefix(path, prefix)
				if idx := strings.IndexByte(seg, '/'); idx >= 0 {
					seg = seg[:idx]
				}
				vid = seg
				break
			}
		}
	}
	if ytIDPattern.MatchString(vid) {
		id := vid
		out.PlatformContentID = &id
		if strings.HasPrefix(path, "/shorts/") {
			out.CanonicalURL = "https://www.youtube.com/shorts/" + id
		} else {
			out.CanonicalURL = "https://www.youtube.com/watch?v=" + id
		}
		return
	}
	out.CanonicalURL = fallbackCleanURLWithHost(u, "www.youtube.com")
}

func normalizeTikTok(u *url.URL, host string, out *NormalizedLink) {
	path := strings.TrimRight(u.Path, "/")
	if m := tiktokVideoID.FindStringSubmatch(path); len(m) == 3 {
		handle := strings.ToLower(m[1])
		id := m[2]
		out.PlatformContentID = &id
		out.CanonicalURL = fmt.Sprintf("https://www.tiktok.com/%s/video/%s", handle, id)
		return
	}
	if m := tiktokEmbedOrV.FindStringSubmatch(path); len(m) == 2 {
		id := m[1]
		out.PlatformContentID = &id
		out.CanonicalURL = fmt.Sprintf("https://www.tiktok.com/video/%s", id)
		return
	}
	if (host == "vm.tiktok.com" || host == "vt.tiktok.com" || strings.HasPrefix(path, "/t/")) && tiktokShortPath.MatchString(path) {
		// Short share links do not expose the numeric video ID until resolved; keep canonical short URL without tracking.
		out.CanonicalURL = fallbackCleanURLWithHost(u, host)
		return
	}
	out.CanonicalURL = fallbackCleanURLWithHost(u, "www.tiktok.com")
}

func normalizeInstagram(u *url.URL, out *NormalizedLink) {
	path := strings.TrimRight(u.Path, "/")
	if m := igPostPattern.FindStringSubmatch(path); len(m) == 3 {
		kind := m[1]
		if kind == "reels" {
			kind = "reel"
		}
		shortcode := m[2]
		out.PlatformContentID = &shortcode
		out.CanonicalURL = fmt.Sprintf("https://www.instagram.com/%s/%s/", kind, shortcode)
		return
	}
	out.CanonicalURL = fallbackCleanURLWithHost(u, "www.instagram.com")
}

func normalizeFacebook(u *url.URL, host string, out *NormalizedLink) {
	path := strings.TrimRight(u.Path, "/")
	if host == "fb.watch" {
		code := strings.Trim(path, "/")
		if code != "" {
			out.CanonicalURL = "https://fb.watch/" + code + "/"
			return
		}
	}
	if v := u.Query().Get("v"); v != "" && (path == "/watch" || path == "/ video.php" || path == "/video.php" || path == "") {
		id := v
		out.PlatformContentID = &id
		out.CanonicalURL = "https://www.facebook.com/watch/?v=" + url.QueryEscape(id)
		return
	}
	if m := fbVideosPattern.FindStringSubmatch(path); len(m) == 2 {
		id := m[1]
		out.PlatformContentID = &id
		if strings.Contains(path, "/reel/") {
			out.CanonicalURL = "https://www.facebook.com/reel/" + id
		} else {
			out.CanonicalURL = "https://www.facebook.com/watch/?v=" + id
		}
		return
	}
	if m := fbSharePattern.FindStringSubmatch(path); len(m) == 2 {
		out.CanonicalURL = fallbackCleanURLWithHost(u, "www.facebook.com")
		return
	}
	out.CanonicalURL = fallbackCleanURLWithHost(u, "www.facebook.com")
}

func extractOffsetMs(u *url.URL) *int64 {
	q := u.Query()
	for _, key := range []string{"t", "start", "time_continue"} {
		if raw := strings.TrimSpace(q.Get(key)); raw != "" {
			// Guard against TikTok's _t Unix epoch tracking parameter (handled separately as "_t", while "t" is offset).
			if ms, ok := parseOffsetParam(raw); ok {
				return &ms
			}
		}
	}
	if frag := strings.TrimSpace(u.Fragment); strings.HasPrefix(frag, "t=") {
		if ms, ok := parseOffsetParam(strings.TrimPrefix(frag, "t=")); ok {
			return &ms
		}
	}
	return nil
}

func parseOffsetParam(raw string) (int64, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if n, err := strconv.ParseFloat(s, 64); err == nil && n >= 0 && n <= 86400 {
		return int64(n * 1000), true
	}
	if m := ytDurationPattern.FindStringSubmatch(s); len(m) == 4 && (m[1] != "" || m[2] != "" || m[3] != "") {
		hrs, _ := strconv.Atoi(m[1])
		mins, _ := strconv.Atoi(m[2])
		secs, _ := strconv.Atoi(m[3])
		total := int64(hrs*3600+mins*60+secs) * 1000
		if total >= 0 && total <= 86400*1000 {
			return total, true
		}
	}
	return 0, false
}

func fallbackCleanURL(u *url.URL) string {
	return fallbackCleanURLWithHost(u, strings.ToLower(u.Hostname()))
}

func fallbackCleanURLWithHost(u *url.URL, canonicalHost string) string {
	cleanQuery := url.Values{}
	for k, vals := range u.Query() {
		lk := strings.ToLower(k)
		if knownTrackingParams[lk] || strings.HasPrefix(lk, "utm_") || lk == "t" || lk == "start" || lk == "time_continue" {
			continue
		}
		cleanQuery[k] = vals
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	out := url.URL{
		Scheme:   "https",
		Host:     canonicalHost,
		Path:     path,
		RawQuery: cleanQuery.Encode(),
	}
	return out.String()
}
