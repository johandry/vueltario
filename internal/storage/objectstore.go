package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ObjectStore abstracts cloud video blob storage & short-lived external staging URLs for Meta's Instagram URL-fetch flow.
// Implementations:
//   - R2Store (Cloudflare R2 via S3-compatible AWS Signature Version 4 — 0 egress fees, recommended for videos)
//   - SupabaseStorage (Supabase Storage REST API + signed URLs)
type ObjectStore interface {
	ProviderName() string
	Upload(ctx context.Context, objectKey, contentType string, body []byte) error
	PresignGetURL(ctx context.Context, objectKey string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, objectKey string) error
}

// --- Cloudflare R2 / S3-Compatible Store (Pure Go AWS SigV4, Zero External SDK Bloat) ---

type R2Config struct {
	AccountID       string // Used if Endpoint is empty: https://<AccountID>.r2.cloudflarestorage.com
	Endpoint        string // e.g. https://<AccountID>.r2.cloudflarestorage.com
	Bucket          string
	Region          string // defaults to "auto" for Cloudflare R2
	AccessKeyID     string
	SecretAccessKey string
	PublicBaseURL   string // optional custom R2 public domain if bucket/prefix is public
	HTTPClient      *http.Client
}

type R2Store struct {
	endpoint        *url.URL
	bucket          string
	region          string
	accessKeyID     string
	secretAccessKey string
	publicBaseURL   string
	client          *http.Client
}

func NewR2Store(cfg R2Config) (*R2Store, error) {
	ep := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if ep == "" && strings.TrimSpace(cfg.AccountID) != "" {
		ep = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", strings.TrimSpace(cfg.AccountID))
	}
	if ep == "" || strings.TrimSpace(cfg.Bucket) == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("r2 store requires endpoint/account_id, bucket, access_key_id, and secret_access_key")
	}
	parsed, err := url.Parse(ep)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid r2 endpoint %q", ep)
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "auto"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	return &R2Store{
		endpoint:        parsed,
		bucket:          strings.TrimSpace(cfg.Bucket),
		region:          region,
		accessKeyID:     strings.TrimSpace(cfg.AccessKeyID),
		secretAccessKey: strings.TrimSpace(cfg.SecretAccessKey),
		publicBaseURL:   strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/"),
		client:          hc,
	}, nil
}

func (r *R2Store) ProviderName() string { return "r2" }

func (r *R2Store) Upload(ctx context.Context, objectKey, contentType string, body []byte) error {
	targetURL := r.objectURL(objectKey)
	payloadHash := sha256Hex(body)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, targetURL.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "video/mp4"
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Host", targetURL.Host)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)

	signedHeaders := "content-type;host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := fmt.Sprintf("content-type:%s\nhost:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		contentType, targetURL.Host, payloadHash, amzDate)

	canonicalReq := strings.Join([]string{
		http.MethodPut,
		awsURIPath(targetURL.Path),
		"",
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, r.region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalReq)),
	}, "\n")

	sig := hex.EncodeToString(hmacSHA256(r.signingKey(dateStamp), []byte(stringToSign)))
	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		r.accessKeyID, scope, signedHeaders, sig)
	req.Header.Set("Authorization", authHeader)

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("r2 upload HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// PresignGetURL generates a time-bounded AWS SigV4 presigned GET URL so Meta's Instagram Content Publishing API
// (or an authenticated desktop/mobile player) can fetch private R2 video bytes with $0 egress cost, expiring automatically.
func (r *R2Store) PresignGetURL(_ context.Context, objectKey string, ttl time.Duration) (string, error) {
	secs := int(ttl.Seconds())
	if secs <= 0 {
		secs = 3600
	}
	if secs > 604800 {
		secs = 604800 // S3 SigV4 max 7 days
	}

	targetURL := r.objectURL(objectKey)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, r.region)

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", fmt.Sprintf("%s/%s", r.accessKeyID, scope))
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(secs))
	q.Set("X-Amz-SignedHeaders", "host")

	canonicalQuery := awsCanonicalQuery(q)
	canonicalHeaders := fmt.Sprintf("host:%s\n", targetURL.Host)
	canonicalReq := strings.Join([]string{
		http.MethodGet,
		awsURIPath(targetURL.Path),
		canonicalQuery,
		canonicalHeaders,
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalReq)),
	}, "\n")

	sig := hex.EncodeToString(hmacSHA256(r.signingKey(dateStamp), []byte(stringToSign)))
	return fmt.Sprintf("%s://%s%s?%s&X-Amz-Signature=%s",
		targetURL.Scheme, targetURL.Host, awsURIPath(targetURL.Path), canonicalQuery, sig), nil
}

func (r *R2Store) Delete(ctx context.Context, objectKey string) error {
	targetURL := r.objectURL(objectKey)
	payloadHash := sha256Hex(nil)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, targetURL.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Host", targetURL.Host)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		targetURL.Host, payloadHash, amzDate)
	canonicalReq := strings.Join([]string{
		http.MethodDelete,
		awsURIPath(targetURL.Path),
		"",
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, r.region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalReq)),
	}, "\n")
	sig := hex.EncodeToString(hmacSHA256(r.signingKey(dateStamp), []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		r.accessKeyID, scope, signedHeaders, sig))

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("r2 delete HTTP %d", resp.StatusCode)
	}
	return nil
}

func (r *R2Store) objectURL(objectKey string) *url.URL {
	cleanKey := strings.TrimLeft(objectKey, "/")
	cp := *r.endpoint
	basePath := strings.TrimRight(cp.Path, "/")
	cp.Path = fmt.Sprintf("%s/%s/%s", basePath, r.bucket, cleanKey)
	return &cp
}

func (r *R2Store) signingKey(dateStamp string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+r.secretAccessKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(r.region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// --- Supabase Storage Adapter (Fallback / Single-Dashboard Option) ---

type SupabaseStorage struct {
	supabaseURL string
	bucket      string
	bearerToken string // Admin JWT or service_role key
	apiKey      string // Supabase anon/service key
	client      *http.Client
}

func NewSupabaseStorage(supabaseURL, apiKey, bearerToken, bucket string) (*SupabaseStorage, error) {
	u := strings.TrimRight(strings.TrimSpace(supabaseURL), "/")
	if u == "" || apiKey == "" || bucket == "" {
		return nil, fmt.Errorf("supabase storage requires url, api_key, and bucket")
	}
	if bearerToken == "" {
		bearerToken = apiKey
	}
	return &SupabaseStorage{
		supabaseURL: u,
		bucket:      strings.TrimSpace(bucket),
		bearerToken: bearerToken,
		apiKey:      apiKey,
		client:      &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

func (s *SupabaseStorage) ProviderName() string { return "supabase" }

func (s *SupabaseStorage) Upload(ctx context.Context, objectKey, contentType string, body []byte) error {
	cleanKey := strings.TrimLeft(objectKey, "/")
	endpoint := fmt.Sprintf("%s/storage/v1/object/%s/%s", s.supabaseURL, url.PathEscape(s.bucket), cleanKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "video/mp4"
	}
	req.Header.Set("apikey", s.apiKey)
	req.Header.Set("Authorization", "Bearer "+s.bearerToken)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-upsert", "true")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("supabase storage upload HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (s *SupabaseStorage) PresignGetURL(ctx context.Context, objectKey string, ttl time.Duration) (string, error) {
	secs := int(ttl.Seconds())
	if secs <= 0 {
		secs = 3600
	}
	cleanKey := strings.TrimLeft(objectKey, "/")
	endpoint := fmt.Sprintf("%s/storage/v1/object/sign/%s/%s", s.supabaseURL, url.PathEscape(s.bucket), cleanKey)
	payload, _ := json.Marshal(map[string]int{"expiresIn": secs})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("apikey", s.apiKey)
	req.Header.Set("Authorization", "Bearer "+s.bearerToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("supabase storage sign HTTP %d", resp.StatusCode)
	}
	var out struct {
		SignedURL string `json:"signedURL"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.SignedURL == "" {
		return "", fmt.Errorf("supabase storage returned empty signedURL")
	}
	if strings.HasPrefix(out.SignedURL, "http") {
		return out.SignedURL, nil
	}
	return s.supabaseURL + "/storage/v1" + out.SignedURL, nil
}

func (s *SupabaseStorage) Delete(ctx context.Context, objectKey string) error {
	cleanKey := strings.TrimLeft(objectKey, "/")
	endpoint := fmt.Sprintf("%s/storage/v1/object/%s/%s", s.supabaseURL, url.PathEscape(s.bucket), cleanKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", s.apiKey)
	req.Header.Set("Authorization", "Bearer "+s.bearerToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// --- Crypto & URI Encoding Helpers ---

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func awsEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func awsURIPath(path string) string {
	if path == "" {
		return "/"
	}
	return awsEncode(path, false)
}

func awsCanonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			pairs = append(pairs, awsEncode(k, true)+"="+awsEncode(v, true))
		}
	}
	return strings.Join(pairs, "&")
}
