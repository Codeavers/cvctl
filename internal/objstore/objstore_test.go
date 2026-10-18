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

func TestProbeBucketsHappyPath(t *testing.T) {
	memStores := map[string]*memStore{
		"products":      newMemStore(),
		"prescriptions": newMemStore(),
	}
	factory := func(c Config) (Store, error) {
		s, ok := memStores[c.Bucket]
		if !ok {
			return nil, errors.New("unknown bucket: " + c.Bucket)
		}
		return s, nil
	}

	var publicGets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		publicGets = append(publicGets, key)
		data, ok := memStores["products"].objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	checks := []BucketCheck{
		{Name: "products", PublicURL: srv.URL},
		{Name: "prescriptions", PublicURL: ""},
	}
	base := Config{
		Endpoint:  "https://s3.example.com",
		Region:    "us-east-1",
		AccessKey: "testaccess",
		SecretKey: "testsecret",
	}

	if err := probeBuckets(context.Background(), base, checks, srv.Client(), factory); err != nil {
		t.Fatalf("ProbeBuckets failed: %v", err)
	}

	for name, s := range memStores {
		if len(s.putKeys) != 1 || len(s.getKeys) != 1 || len(s.delKeys) != 1 {
			t.Fatalf("bucket %s expected 1 put/get/del, got put=%d get=%d del=%d", name, len(s.putKeys), len(s.getKeys), len(s.delKeys))
		}
		if !strings.HasPrefix(s.putKeys[0], "_cvctl-probe-") {
			t.Fatalf("bucket %s probe object name should start with _cvctl-probe-, got %q", name, s.putKeys[0])
		}
		hexPart := strings.TrimPrefix(s.putKeys[0], "_cvctl-probe-")
		if len(hexPart) != 16 {
			t.Fatalf("bucket %s expected 16 hex chars after _cvctl-probe-, got %q", name, hexPart)
		}
		if len(s.objects) != 0 {
			t.Fatalf("bucket %s probe object was not cleaned up: %v", name, s.objects)
		}
	}

	if len(publicGets) != 1 {
		t.Fatalf("expected 1 public GET for products, got %d", len(publicGets))
	}
	if publicGets[0] != memStores["products"].putKeys[0] {
		t.Fatalf("public GET requested key %q, want %q", publicGets[0], memStores["products"].putKeys[0])
	}
}

func TestProbeBucketsPublicGetWrongBody(t *testing.T) {
	mem := newMemStore()
	factory := func(c Config) (Store, error) { return mem, nil }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("wrong-body-content"))
	}))
	defer srv.Close()

	checks := []BucketCheck{{Name: "products", PublicURL: srv.URL}}
	base := Config{Endpoint: "https://s3.example.com"}

	err := probeBuckets(context.Background(), base, checks, srv.Client(), factory)
	if err == nil {
		t.Fatal("expected error on wrong public body, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T (%v)", err, err)
	}
	if oe.Code != "E_S3_PROBE" {
		t.Fatalf("code = %q, want E_S3_PROBE", oe.Code)
	}
	if !strings.Contains(oe.Message, "bucket products: public-get:") {
		t.Fatalf("message = %q, want bucket products: public-get: ...", oe.Message)
	}
	if oe.Hint != "check the bucket name, the app keys and the public URL: cvctl project update <inst>" {
		t.Fatalf("hint = %q", oe.Hint)
	}
	if len(mem.objects) != 0 {
		t.Fatalf("probe object should be deleted on failure, remaining: %v", mem.objects)
	}
}

func TestProbeBucketsPublicGet403(t *testing.T) {
	mem := newMemStore()
	factory := func(c Config) (Store, error) { return mem, nil }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Forbidden"))
	}))
	defer srv.Close()

	checks := []BucketCheck{{Name: "products", PublicURL: srv.URL}}
	base := Config{Endpoint: "https://s3.example.com"}

	err := probeBuckets(context.Background(), base, checks, srv.Client(), factory)
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T (%v)", err, err)
	}
	if oe.Code != "E_S3_PROBE" {
		t.Fatalf("code = %q, want E_S3_PROBE", oe.Code)
	}
	if !strings.Contains(oe.Message, "bucket products: public-get:") || !strings.Contains(oe.Message, "403") {
		t.Fatalf("message = %q, want bucket products: public-get: ... 403", oe.Message)
	}
	if len(mem.objects) != 0 {
		t.Fatalf("probe object should be deleted on failure, remaining: %v", mem.objects)
	}
}

func TestProbeBucketsSecretNeverLeaked(t *testing.T) {
	const (
		accessKey = "AKIASECRETACCESSKEY99"
		secretKey = "superSecretKeyValueThatMustNeverLeakUnderAnyCircumstances"
	)

	t.Run("error message containing secret", func(t *testing.T) {
		mem := newMemStore()
		mem.putErr = errors.New("connection failed with secret " + secretKey + " and key " + accessKey)
		factory := func(c Config) (Store, error) { return mem, nil }

		base := Config{
			Endpoint:  "https://s3.example.com",
			AccessKey: accessKey,
			SecretKey: secretKey,
		}
		err := probeBuckets(context.Background(), base, []BucketCheck{{Name: "b1"}}, nil, factory)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var oe *output.Error
		if !errors.As(err, &oe) {
			t.Fatalf("expected *output.Error, got %T", err)
		}
		if oe.Code != "E_S3_PROBE" {
			t.Fatalf("code = %q, want E_S3_PROBE", oe.Code)
		}
		fullText := oe.Error() + " " + oe.Hint
		if strings.Contains(fullText, secretKey) {
			t.Fatalf("secret key leaked in error: %q", fullText)
		}
		if strings.Contains(fullText, accessKey) {
			t.Fatalf("access key leaked in error: %q", fullText)
		}
	})

	t.Run("403 AccessDenied from real client against fake S3", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied.</Message><RequestId>1</RequestId></Error>`))
		}))
		defer srv.Close()

		base := Config{
			Endpoint:  srv.URL,
			Region:    "us-east-1",
			AccessKey: accessKey,
			SecretKey: secretKey,
		}
		err := ProbeBuckets(context.Background(), base, []BucketCheck{{Name: "bucket1"}}, srv.Client())
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var oe *output.Error
		if !errors.As(err, &oe) {
			t.Fatalf("expected *output.Error, got %T", err)
		}
		if oe.Code != "E_S3_PROBE" {
			t.Fatalf("code = %q, want E_S3_PROBE", oe.Code)
		}
		if !strings.Contains(oe.Message, "bucket bucket1: put: AccessDenied") {
			t.Fatalf("message = %q, want bucket bucket1: put: AccessDenied", oe.Message)
		}
		fullText := oe.Error() + " " + oe.Hint
		if strings.Contains(fullText, secretKey) || strings.Contains(fullText, accessKey) {
			t.Fatalf("credentials leaked: %q", fullText)
		}
		if strings.Contains(fullText, "X-Amz-Signature") || strings.Contains(fullText, "?") {
			t.Fatalf("query string/signature leaked: %q", fullText)
		}
	})
}

func TestProbeBucketsStepsErrorMapping(t *testing.T) {
	base := Config{Endpoint: "https://s3.example.com"}

	t.Run("put error", func(t *testing.T) {
		mem := newMemStore()
		mem.putErr = errors.New("disk full")
		err := probeBuckets(context.Background(), base, []BucketCheck{{Name: "b1"}}, nil, func(c Config) (Store, error) { return mem, nil })
		var oe *output.Error
		if !errors.As(err, &oe) || !strings.Contains(oe.Message, "bucket b1: put: disk full") {
			t.Fatalf("expected put error, got %v", err)
		}
	})

	t.Run("get error", func(t *testing.T) {
		mem := newMemStore()
		mem.getErr = errors.New("read timeout")
		err := probeBuckets(context.Background(), base, []BucketCheck{{Name: "b1"}}, nil, func(c Config) (Store, error) { return mem, nil })
		var oe *output.Error
		if !errors.As(err, &oe) || !strings.Contains(oe.Message, "bucket b1: get: read timeout") {
			t.Fatalf("expected get error, got %v", err)
		}
		if len(mem.objects) != 0 {
			t.Fatalf("expected object deleted on get failure, remaining: %v", mem.objects)
		}
	})

	t.Run("compare error", func(t *testing.T) {
		mem := newMemStore()
		factory := func(c Config) (Store, error) { return &corruptGetStore{memStore: mem}, nil }
		err := probeBuckets(context.Background(), base, []BucketCheck{{Name: "b1"}}, nil, factory)
		var oe *output.Error
		if !errors.As(err, &oe) || !strings.Contains(oe.Message, "bucket b1: compare: content mismatch") {
			t.Fatalf("expected compare error, got %v", err)
		}
		if len(mem.objects) != 0 {
			t.Fatalf("expected object deleted on compare failure, remaining: %v", mem.objects)
		}
	})

	t.Run("delete error", func(t *testing.T) {
		mem := newMemStore()
		mem.delErr = errors.New("permission denied")
		err := probeBuckets(context.Background(), base, []BucketCheck{{Name: "b1"}}, nil, func(c Config) (Store, error) { return mem, nil })
		var oe *output.Error
		if !errors.As(err, &oe) || !strings.Contains(oe.Message, "bucket b1: delete: permission denied") {
			t.Fatalf("expected delete error, got %v", err)
		}
	})

	t.Run("redirect rejected in public get", func(t *testing.T) {
		mem := newMemStore()
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer target.Close()

		redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusMovedPermanently)
		}))
		defer redirectSrv.Close()

		err := probeBuckets(context.Background(), base, []BucketCheck{{Name: "b1", PublicURL: redirectSrv.URL}}, redirectSrv.Client(), func(c Config) (Store, error) { return mem, nil })
		var oe *output.Error
		if !errors.As(err, &oe) || !strings.Contains(oe.Message, "bucket b1: public-get: HTTP 301") {
			t.Fatalf("expected 301 error without redirect, got %v", err)
		}
	})
}

type corruptGetStore struct {
	*memStore
}

func (c *corruptGetStore) Get(ctx context.Context, key string) ([]byte, error) {
	return []byte("corrupt-data"), nil
}
