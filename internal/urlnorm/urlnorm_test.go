package urlnorm

import (
	"errors"
	"testing"

	"github.com/johandry/vueltario/internal/domain"
)

func TestNormalizeRepresentativeLinks(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		platform   domain.Platform
		contentID  string
		canonical  string
		offsetMs   int64
		hasOffset  bool
	}{
		{
			name:      "youtube watch with tracking and offset",
			raw:       "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=1m12s&utm_source=ig&si=xyz",
			platform:  domain.PlatformYouTube,
			contentID: "dQw4w9WgXcQ",
			canonical: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			offsetMs:  72000,
			hasOffset: true,
		},
		{
			name:      "youtu.be short link",
			raw:       "https://youtu.be/M7lc1UVf-VE?start=15&feature=shared",
			platform:  domain.PlatformYouTube,
			contentID: "M7lc1UVf-VE",
			canonical: "https://www.youtube.com/watch?v=M7lc1UVf-VE",
			offsetMs:  15000,
			hasOffset: true,
		},
		{
			name:      "youtube shorts",
			raw:       "https://m.youtube.com/shorts/abcdefghijk?si=123",
			platform:  domain.PlatformYouTube,
			contentID: "abcdefghijk",
			canonical: "https://www.youtube.com/shorts/abcdefghijk",
		},
		{
			name:      "tiktok video url strips _t and _r tracking",
			raw:       "https://www.tiktok.com/@CasinoTimba/video/7345678901234567890?_t=1712345678&_r=1",
			platform:  domain.PlatformTikTok,
			contentID: "7345678901234567890",
			canonical: "https://www.tiktok.com/@casinotimba/video/7345678901234567890",
		},
		{
			name:      "instagram reel strips igsh",
			raw:       "https://www.instagram.com/reels/C8abcXYZ123/?igsh=MzRlODBiNWFlZA==",
			platform:  domain.PlatformInstagram,
			contentID: "C8abcXYZ123",
			canonical: "https://www.instagram.com/reel/C8abcXYZ123/",
		},
		{
			name:      "facebook watch strips fbclid",
			raw:       "https://www.facebook.com/watch/?v=10153231379946729&fbclid=IwAR999",
			platform:  domain.PlatformFacebook,
			contentID: "10153231379946729",
			canonical: "https://www.facebook.com/watch/?v=10153231379946729",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.OriginalURL != tc.raw {
				t.Errorf("original URL not preserved: got %q, want %q", got.OriginalURL, tc.raw)
			}
			if got.Platform != tc.platform {
				t.Errorf("platform: got %q, want %q", got.Platform, tc.platform)
			}
			if tc.contentID != "" {
				if got.PlatformContentID == nil || *got.PlatformContentID != tc.contentID {
					t.Errorf("content ID: got %v, want %q", got.PlatformContentID, tc.contentID)
				}
			}
			if got.CanonicalURL != tc.canonical {
				t.Errorf("canonical URL: got %q, want %q", got.CanonicalURL, tc.canonical)
			}
			if tc.hasOffset {
				if got.ObservedOffsetMs == nil || *got.ObservedOffsetMs != tc.offsetMs {
					t.Errorf("offset: got %v, want %d", got.ObservedOffsetMs, tc.offsetMs)
				}
			}
		})
	}
}

func TestNormalizeUnsupportedHostPreservesInput(t *testing.T) {
	raw := "https://vimeo.com/123456789"
	got, err := Normalize(raw)
	if !errors.Is(err, domain.ErrUnsupportedHost) {
		t.Fatalf("expected ErrUnsupportedHost, got %v", err)
	}
	if got.OriginalURL != raw {
		t.Fatalf("expected OriginalURL %q preserved on error, got %q", raw, got.OriginalURL)
	}
}

func TestExtractSupportedURLsFromText(t *testing.T) {
	shared := "Check these casino moves: https://youtu.be/dQw4w9WgXcQ, https://vimeo.com/111 and https://www.instagram.com/reel/C8abcXYZ123/!"
	links := ExtractSupportedURLsFromText(shared)
	if len(links) != 2 {
		t.Fatalf("expected 2 supported links, got %d", len(links))
	}
	if links[0].Platform != domain.PlatformYouTube || links[1].Platform != domain.PlatformInstagram {
		t.Fatalf("unexpected platforms: %+v", links)
	}
}
