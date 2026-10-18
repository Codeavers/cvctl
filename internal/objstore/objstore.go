// Package objstore provides an S3-compatible object store client and a
// round-trip readiness probe.
package objstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/codeavers/cvctl/internal/output"
)

type Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
}

type Store interface {
	EnsureBucket(ctx context.Context) error
	Put(ctx context.Context, key string, b []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

type minioStore struct {
	client         *minio.Client
	cfg            Config
	endpointOrigin string
}

// New creates a Store backed by minio-go v7. An http:// endpoint disables TLS.
func New(c Config) (Store, error) {
	if strings.TrimSpace(c.Endpoint) == "" {
		return nil, &output.Error{
			Code:    "E_STORAGE_CHECK",
			Message: "storage endpoint is empty",
			Hint:    "set CVCTL_S3_ENDPOINT in /etc/cvctl/host.env",
		}
	}

	var (
		host           string
		secure         bool
		endpointOrigin string
	)
	if strings.Contains(c.Endpoint, "://") {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, &output.Error{
				Code:    "E_STORAGE_CHECK",
				Message: "invalid storage endpoint scheme or host",
				Hint:    "set CVCTL_S3_ENDPOINT to http://host:port or https://host",
			}
		}
		host = u.Host
		secure = u.Scheme == "https"
		endpointOrigin = u.Scheme + "://" + u.Host
	} else {
		host = c.Endpoint
		secure = true
		endpointOrigin = "https://" + c.Endpoint
	}

	region := c.Region
	if region == "" {
		region = "us-east-1"
	}

	cl, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, &output.Error{
			Code:    "E_STORAGE_CHECK",
			Message: fmt.Sprintf("cannot initialize storage client for %s", endpointOrigin),
			Hint:    "check CVCTL_S3_ENDPOINT in /etc/cvctl/host.env",
		}
	}

	return &minioStore{
		client:         cl,
		cfg:            c,
		endpointOrigin: endpointOrigin,
	}, nil
}

func (s *minioStore) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.cfg.Bucket)
	if err != nil {
		return s.mapErr("ensure_bucket", err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, s.cfg.Bucket, minio.MakeBucketOptions{Region: s.cfg.Region}); err != nil {
		return s.mapErr("ensure_bucket", err)
	}
	return nil
}

func (s *minioStore) Put(ctx context.Context, key string, b []byte) error {
	_, err := s.client.PutObject(ctx, s.cfg.Bucket, key, bytes.NewReader(b), int64(len(b)), minio.PutObjectOptions{})
	if err != nil {
		return s.mapErr("put", err)
	}
	return nil
}

func (s *minioStore) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, s.mapErr("get", err)
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, s.mapErr("get", err)
	}
	return data, nil
}

func (s *minioStore) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return s.mapErr("delete", err)
	}
	return nil
}

func (s *minioStore) mapErr(op string, err error) error {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe
	}
	er := minio.ToErrorResponse(err)
	if er.Code != "" {
		return &output.Error{
			Code:    "E_STORAGE_CHECK",
			Message: fmt.Sprintf("storage %s: %s", op, er.Code),
			Hint:    "check S3 endpoint, bucket and credentials",
		}
	}
	if er.StatusCode != 0 {
		return &output.Error{
			Code:    "E_STORAGE_CHECK",
			Message: fmt.Sprintf("storage %s: HTTP %d", op, er.StatusCode),
			Hint:    "check S3 endpoint, bucket and credentials",
		}
	}
	return &output.Error{
		Code:    "E_STORAGE_CHECK",
		Message: "cannot reach storage endpoint " + s.endpointOrigin,
		Hint:    "check network connectivity and S3 endpoint",
	}
}

// Probe writes, reads back, compares and deletes a random object under cvctl-check/.
func Probe(ctx context.Context, s Store) error {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return probeErr("random", err)
	}
	hexID := hex.EncodeToString(rnd[:])
	key := "cvctl-check/" + hexID
	payload := []byte("cvctl-probe-" + hexID)

	if err := s.Put(ctx, key, payload); err != nil {
		return probeErr("put", err)
	}
	got, err := s.Get(ctx, key)
	if err != nil {
		_ = s.Delete(ctx, key)
		return probeErr("get", err)
	}
	if !bytes.Equal(got, payload) {
		_ = s.Delete(ctx, key)
		return &output.Error{
			Code:    "E_STORAGE_CHECK",
			Message: "storage probe: round-trip content mismatch",
			Hint:    "check S3 endpoint and bucket integrity",
		}
	}
	if err := s.Delete(ctx, key); err != nil {
		return probeErr("delete", err)
	}
	return nil
}

func probeErr(op string, err error) error {
	var oe *output.Error
	if errors.As(err, &oe) {
		if oe.Code == "" {
			oe.Code = "E_STORAGE_CHECK"
		}
		return oe
	}
	return &output.Error{
		Code:    "E_STORAGE_CHECK",
		Message: fmt.Sprintf("storage %s: %v", op, err),
		Hint:    "check S3 endpoint, bucket and credentials",
	}
}

// BucketCheck pairs a bucket name with its optional public URL.
type BucketCheck struct {
	Name      string
	PublicURL string
}

// ProbeBuckets checks each bucket by putting, reading back, comparing and
// deleting an object. When PublicURL is non-empty, it also performs an
// unsigned GET against the public URL.
func ProbeBuckets(ctx context.Context, base Config, buckets []BucketCheck, hc *http.Client) error {
	return probeBuckets(ctx, base, buckets, hc, New)
}

func probeBuckets(ctx context.Context, base Config, buckets []BucketCheck, hc *http.Client, newStore func(Config) (Store, error)) error {
	if newStore == nil {
		newStore = New
	}
	var client http.Client
	if hc != nil {
		client = *hc
	}
	if client.Timeout == 0 {
		client.Timeout = 10 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	for _, b := range buckets {
		cfg := base
		cfg.Bucket = b.Name
		store, err := newStore(cfg)
		if err != nil {
			return probeBucketErr(b.Name, "put", err, base)
		}

		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return probeBucketErr(b.Name, "put", err, base)
		}
		hexID := hex.EncodeToString(rnd[:])
		objKey := "_cvctl-probe-" + hexID
		payload := []byte(objKey)

		if err := store.Put(ctx, objKey, payload); err != nil {
			return probeBucketErr(b.Name, "put", err, base)
		}

		got, err := store.Get(ctx, objKey)
		if err != nil {
			_ = store.Delete(ctx, objKey)
			return probeBucketErr(b.Name, "get", err, base)
		}
		if !bytes.Equal(got, payload) {
			_ = store.Delete(ctx, objKey)
			return probeBucketErr(b.Name, "compare", errors.New("content mismatch"), base)
		}

		if b.PublicURL != "" {
			if err := func() error {
				urlStr := strings.TrimRight(b.PublicURL, "/") + "/" + objKey
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
				if err != nil {
					return probeBucketErr(b.Name, "public-get", err, base)
				}
				resp, err := client.Do(req)
				if err != nil {
					return probeBucketErr(b.Name, "public-get", err, base)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return probeBucketErr(b.Name, "public-get", fmt.Errorf("HTTP %d", resp.StatusCode), base)
				}
				pubBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				if err != nil {
					return probeBucketErr(b.Name, "public-get", err, base)
				}
				if !bytes.Equal(pubBody, payload) {
					return probeBucketErr(b.Name, "public-get", errors.New("content mismatch"), base)
				}
				return nil
			}(); err != nil {
				_ = store.Delete(ctx, objKey)
				return err
			}
		}

		if err := store.Delete(ctx, objKey); err != nil {
			return probeBucketErr(b.Name, "delete", err, base)
		}
	}
	return nil
}

func probeBucketErr(bucket, step string, err error, base Config) error {
	var oe *output.Error
	if errors.As(err, &oe) && oe.Code == "E_S3_PROBE" {
		return oe
	}
	var reason string
	if errors.As(err, &oe) {
		reason = oe.Message
	} else {
		er := minio.ToErrorResponse(err)
		if er.Code != "" {
			reason = er.Code
		} else if er.StatusCode != 0 {
			reason = fmt.Sprintf("HTTP %d", er.StatusCode)
		} else {
			reason = err.Error()
		}
	}
	reason = strings.TrimPrefix(reason, "storage "+step+": ")
	reason = strings.TrimPrefix(reason, "storage: ")
	reason = sanitizeReason(reason, base)

	return &output.Error{
		Code:    "E_S3_PROBE",
		Message: fmt.Sprintf("bucket %s: %s: %s", bucket, step, reason),
		Hint:    "check the bucket name, the app keys and the public URL: cvctl project update <inst>",
	}
}

func sanitizeReason(reason string, base Config) string {
	for {
		idx := strings.Index(reason, "?")
		if idx == -1 {
			break
		}
		end := strings.IndexAny(reason[idx:], " \t\n\r\"'>")
		if end == -1 {
			reason = reason[:idx]
			break
		}
		reason = reason[:idx] + reason[idx+end:]
	}
	if base.SecretKey != "" {
		reason = strings.ReplaceAll(reason, base.SecretKey, "***")
	}
	if base.AccessKey != "" {
		reason = strings.ReplaceAll(reason, base.AccessKey, "***")
	}
	return strings.TrimSpace(reason)
}
