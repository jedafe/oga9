// Ref: oga9-engineering/sections/14-comms-documents.md + ADR-019 —
// S3-compatible blob backend (SeaweedFS phased in per the stack file).
// SigV4-signed Put/Get/Stat/Remove behind the same Store interface; the
// local filesystem backend stays the default when OGA9_S3_* is unset
// (self-hosted must work without external services, Rule 29).
package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// BlobStore is the backend seam — local disk (Store) or S3-compatible
// (S3Store, e.g. SeaweedFS -s3). Both implement it.
type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (int64, error)
	Remove(ctx context.Context, key string) error
	// PreviewURL returns an imgproxy resize URL for the blob, or "" when
	// previews aren't available (local disk backend / imgproxy unset).
	PreviewURL(key string, w, h int) string
}

// PreviewURL — the local-disk backend has no remote image proxy.
func (s *Store) PreviewURL(string, int, int) string { return "" }

var _ BlobStore = (*Store)(nil)
var _ BlobStore = (*S3Store)(nil)

type S3Store struct {
	endpoint, bucket, ak, sk string
	imgproxy                 string
	hc                       *http.Client
}

func NewS3(endpoint, bucket, accessKey, secretKey string) *S3Store {
	return &S3Store{endpoint: strings.TrimRight(endpoint, "/"), bucket: bucket,
		ak: accessKey, sk: secretKey, hc: &http.Client{Timeout: 30 * time.Second}}
}

// WithImgproxy enables image preview URLs through OGA9_IMGPROXY_URL.
func (s *S3Store) WithImgproxy(base string) *S3Store {
	s.imgproxy = strings.TrimRight(base, "/")
	return s
}

// PreviewURL — imgproxy resize of the stored object; "" when unconfigured
// (self-hosted works fine without imgproxy, Rule 29).
func (s *S3Store) PreviewURL(key string, w, h int) string {
	if s.imgproxy == "" {
		return ""
	}
	return ImgproxyURL(s.imgproxy, s.bucket, key, w, h)
}

func (s *S3Store) url(key string) string {
	return s.endpoint + "/" + s.bucket + "/" + strings.TrimLeft(path.Clean("/"+key), "/")
}

func (s *S3Store) sign(req *http.Request, payload []byte) {
	// SigV4, unsigned-payload variant (HTTPS endpoints hash the body cheaply).
	t := time.Now().UTC()
	req.Header.Set("x-amz-date", t.Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	host := req.URL.Host
	uri := req.URL.EscapedPath()
	// canonical request: Method\nURI\nQuery\nHeaders\nSignedHeaders\nPayload —
	// the headers block already ends with \n, so no empty element follows it.
	canon := req.Method + "\n" + uri + "\n\n" +
		"host:" + host + "\nx-amz-content-sha256:UNSIGNED-PAYLOAD\nx-amz-date:" +
		req.Header.Get("x-amz-date") + "\n\n" +
		"host;x-amz-content-sha256;x-amz-date\n" + "UNSIGNED-PAYLOAD"
	scope := t.Format("20060102") + "/us-east-1/s3/aws4_request"
	h := sha256.Sum256([]byte(canon))
	toSign := "AWS4-HMAC-SHA256\n" + t.Format("20060102T150405Z") + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	mac := func(key []byte, msg string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(msg))
		return m.Sum(nil)
	}
	k := mac(mac(mac(mac([]byte("AWS4"+s.sk), t.Format("20060102")), "us-east-1"), "s3"), "aws4_request")
	sig := hex.EncodeToString(mac(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.ak+"/"+scope+
		", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+sig)
}

func (s *S3Store) do(ctx context.Context, method, key string, body io.Reader) (*http.Response, error) {
	var payload []byte
	req, err := http.NewRequestWithContext(ctx, method, s.url(key), body)
	if err != nil {
		return nil, err
	}
	s.sign(req, payload)
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("s3 %s %s: %d %s", method, key, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (s *S3Store) Put(ctx context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodPut, key, bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *S3Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *S3Store) Stat(ctx context.Context, key string) (int64, error) {
	resp, err := s.do(ctx, http.MethodHead, key, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.ContentLength, nil
}

func (s *S3Store) Remove(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ImgproxyURL builds an unsigned imgproxy resize URL for an s3:// source —
// documents with image MIME get a preview_url this way when
// OGA9_IMGPROXY_URL is configured (dev/imgproxy default is unsigned).
func ImgproxyURL(base, bucket, key string, w, h int) string {
	src := url.QueryEscape("s3://" + bucket + "/" + key)
	return strings.TrimRight(base, "/") +
		fmt.Sprintf("/insecure/resize:fit:%d:%d/plain/%s", w, h, src)
}
