package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestR2StoreSigV4UploadPresignAndDelete(t *testing.T) {
	var gotAuthHeader, gotPutPath, gotDelPath string
	var gotBody []byte

	mockR2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			gotAuthHeader = r.Header.Get("Authorization")
			gotPutPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			gotDelPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer mockR2.Close()

	store, err := NewR2Store(R2Config{
		Endpoint:        mockR2.URL,
		Bucket:          "vueltario-media",
		Region:          "auto",
		AccessKeyID:     "R2_TEST_ACCESS_KEY",
		SecretAccessKey: "R2_TEST_SECRET_KEY",
	})
	if err != nil {
		t.Fatalf("NewR2Store: %v", err)
	}

	payload := []byte("mp4-video-bytes")
	if err := store.Upload(context.Background(), "clips/enchufla.mp4", "video/mp4", payload); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if gotPutPath != "/vueltario-media/clips/enchufla.mp4" {
		t.Fatalf("unexpected PUT path %q", gotPutPath)
	}
	if !strings.HasPrefix(gotAuthHeader, "AWS4-HMAC-SHA256 Credential=R2_TEST_ACCESS_KEY/") {
		t.Fatalf("invalid SigV4 Authorization header: %q", gotAuthHeader)
	}
	if string(gotBody) != string(payload) {
		t.Fatalf("body mismatch")
	}

	presigned, err := store.PresignGetURL(context.Background(), "clips/enchufla.mp4", 30*time.Minute)
	if err != nil {
		t.Fatalf("PresignGetURL failed: %v", err)
	}
	if !strings.Contains(presigned, "X-Amz-Algorithm=AWS4-HMAC-SHA256") ||
		!strings.Contains(presigned, "X-Amz-Expires=1800") ||
		!strings.Contains(presigned, "X-Amz-Signature=") {
		t.Fatalf("unexpected presigned URL: %s", presigned)
	}

	if err := store.Delete(context.Background(), "clips/enchufla.mp4"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if gotDelPath != "/vueltario-media/clips/enchufla.mp4" {
		t.Fatalf("unexpected DELETE path %q", gotDelPath)
	}
}
