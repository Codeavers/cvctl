package objstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/output"
)

type memStore struct {
	objects   map[string][]byte
	putKeys   []string
	getKeys   []string
	delKeys   []string
	putErr    error
	getErr    error
	delErr    error
	bucketErr error
}

func newMemStore() *memStore {
	return &memStore{objects: make(map[string][]byte)}
}

func (m *memStore) EnsureBucket(_ context.Context) error {
	return m.bucketErr
}

func (m *memStore) Put(_ context.Context, key string, b []byte) error {
	if m.putErr != nil {
		return m.putErr
	}
	m.putKeys = append(m.putKeys, key)
	m.objects[key] = append([]byte(nil), b...)
	return nil
}

func (m *memStore) Get(_ context.Context, key string) ([]byte, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	m.getKeys = append(m.getKeys, key)
	b, ok := m.objects[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), b...), nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	if m.delErr != nil {
		return m.delErr
	}
	m.delKeys = append(m.delKeys, key)
	delete(m.objects, key)
	return nil
}

func TestProbeRoundTrip(t *testing.T) {
	s := newMemStore()
	if err := Probe(context.Background(), s); err != nil {
		t.Fatalf("Probe failed: %v", err)
	}
	if len(s.putKeys) != 1 || len(s.getKeys) != 1 || len(s.delKeys) != 1 {
		t.Fatalf("expected 1 put/get/delete, got put=%d get=%d del=%d", len(s.putKeys), len(s.getKeys), len(s.delKeys))
	}
	key := s.putKeys[0]
	if !strings.HasPrefix(key, "cvctl-check/") {
		t.Fatalf("expected key under cvctl-check/, got %q", key)
	}
	hexPart := strings.TrimPrefix(key, "cvctl-check/")
	if len(hexPart) != 16 {
		t.Fatalf("expected 16 random hex chars after cvctl-check/, got %d (%q)", len(hexPart), hexPart)
	}
	if s.getKeys[0] != key || s.delKeys[0] != key {
		t.Fatalf("key mismatch across put/get/delete: %q / %q / %q", key, s.getKeys[0], s.delKeys[0])
	}
	if len(s.objects) != 0 {
		t.Fatalf("expected probe object to be deleted, remaining: %v", s.objects)
	}
}

func TestProbeErrorHasNoSecret(t *testing.T) {
	const (
		accessKey = "AKIASECRETACCESSKEY1"
		secretKey = "superSecretKeyValueThatMustNeverLeakInErrors1234"
	)

	t.Run("403 AccessDenied", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><RequestId>1</RequestId></Error>`))
		}))
		defer srv.Close()

		store, err := New(Config{
			Endpoint:  srv.URL,
			Bucket:    "cvctl",
			Region:    "us-east-1",
			AccessKey: accessKey,
			SecretKey: secretKey,
		})
		if err != nil {
			t.Fatalf("New error: %v", err)
		}

		err = Probe(context.Background(), store)
		if err == nil {
			t.Fatal("expected Probe error on 403, got nil")
		}
		var oe *output.Error
		if !errors.As(err, &oe) {
			t.Fatalf("expected *output.Error, got %T (%v)", err, err)
		}
		if oe.Code != "E_STORAGE_CHECK" {
			t.Fatalf("got code %q, want E_STORAGE_CHECK", oe.Code)
		}
		if !strings.Contains(oe.Message, "AccessDenied") {
			t.Fatalf("expected S3 error code AccessDenied in message, got %q", oe.Message)
		}
		fullText := oe.Error() + " " + oe.Hint
		if strings.Contains(fullText, accessKey) || strings.Contains(fullText, secretKey) {
			t.Fatalf("error leaked credentials: %q", fullText)
		}
		if strings.Contains(fullText, "X-Amz-Signature") || strings.Contains(fullText, "?") {
			t.Fatalf("error leaked query string or signed URL: %q", fullText)
		}
	})

	t.Run("404 NoSuchBucket", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message><RequestId>2</RequestId></Error>`))
		}))
		defer srv.Close()

		store, err := New(Config{
			Endpoint:  srv.URL,
			Bucket:    "missing-bucket",
			Region:    "us-east-1",
			AccessKey: accessKey,
			SecretKey: secretKey,
		})
		if err != nil {
			t.Fatalf("New error: %v", err)
		}

		err = Probe(context.Background(), store)
		if err == nil {
			t.Fatal("expected Probe error on NoSuchBucket, got nil")
		}
		var oe *output.Error
		if !errors.As(err, &oe) {
			t.Fatalf("expected *output.Error, got %T (%v)", err, err)
		}
		if oe.Code != "E_STORAGE_CHECK" {
			t.Fatalf("got code %q, want E_STORAGE_CHECK", oe.Code)
		}
		if !strings.Contains(oe.Message, "NoSuchBucket") {
			t.Fatalf("expected S3 error code NoSuchBucket in message, got %q", oe.Message)
		}
		fullText := oe.Error() + " " + oe.Hint
		if strings.Contains(fullText, accessKey) || strings.Contains(fullText, secretKey) {
			t.Fatalf("error leaked credentials: %q", fullText)
		}
	})

	t.Run("network error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srvURL := srv.URL
		srv.Close()

		store, err := New(Config{
			Endpoint:  srvURL,
			Bucket:    "cvctl",
			Region:    "us-east-1",
			AccessKey: accessKey,
			SecretKey: secretKey,
		})
		if err != nil {
			t.Fatalf("New error: %v", err)
		}

		err = Probe(context.Background(), store)
		if err == nil {
			t.Fatal("expected Probe error on closed server, got nil")
		}
		var oe *output.Error
		if !errors.As(err, &oe) {
			t.Fatalf("expected *output.Error, got %T (%v)", err, err)
		}
		if oe.Code != "E_STORAGE_CHECK" {
			t.Fatalf("got code %q, want E_STORAGE_CHECK", oe.Code)
		}
		wantMsg := "cannot reach storage endpoint " + srvURL
		if oe.Message != wantMsg {
			t.Fatalf("got message %q, want %q", oe.Message, wantMsg)
		}
		fullText := oe.Error() + " " + oe.Hint
		if strings.Contains(fullText, accessKey) || strings.Contains(fullText, secretKey) {
			t.Fatalf("error leaked credentials: %q", fullText)
		}
	})
}
