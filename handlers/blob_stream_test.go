package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func blobRequest(t *testing.T, origin string, data []byte) *http.Request {
	t.Helper()
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	req, err := http.NewRequest(http.MethodGet, origin+"/v2/team/app/blobs/sha256:"+digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer private-test-token")
	return req
}

func TestRegistryBlobRecoversTruncatedAndStalledResponses(t *testing.T) {
	data := bytes.Repeat([]byte("immutable-layer"), 8192)
	for _, mode := range []string{"full", "client-range", "stalled"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			start, end := 0, len(data)-1
			if mode == "client-range" {
				start, end = 97, 35000
			}
			cut := start + 4096
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer private-test-token" {
					t.Error("retry lost registry authentication")
				}
				if n == 1 {
					w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
					if mode == "client-range" {
						w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
						w.WriteHeader(http.StatusPartialContent)
					}
					w.Write(data[start:cut])
					w.(http.Flusher).Flush()
					if mode == "stalled" {
						<-r.Context().Done()
					}
					return // Closing before Content-Length reproduces the production EOF.
				}
				if got, want := r.Header.Get("Range"), fmt.Sprintf("bytes=%d-%d", cut, end); got != want {
					t.Errorf("resume Range = %q, want %q", got, want)
				}
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("range retry must request unchanged blob bytes")
				}
				w.Header().Set("Content-Length", strconv.Itoa(end-cut+1))
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", cut, end, len(data)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(data[cut : end+1])
			}))
			defer upstream.Close()
			req := blobRequest(t, upstream.URL, data)
			if mode == "client-range" {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
			}
			resp, cancel, err := openRegistryBlob(upstream.Client(), req, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			var dst bytes.Buffer
			result, err := streamRegistryBlob(req.Context(), &dst, upstream.Client(), req, resp, cancel, 100*time.Millisecond)
			if err != nil || result.resumes != 1 || calls.Load() != 2 {
				t.Fatalf("stream = %+v, err=%v, calls=%d", result, err, calls.Load())
			}
			if !bytes.Equal(dst.Bytes(), data[start:end+1]) {
				t.Fatal("resumed stream has missing or duplicate bytes")
			}
		})
	}
}

func TestRegistryBlobRejectsUnsafeResumeResponses(t *testing.T) {
	for _, mode := range []string{"full-200", "wrong-offset", "changed-total", "wrong-length", "auth-failure"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			data := []byte("0123456789")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Length", "10")
					w.Write(data[:3])
					return
				}
				switch mode {
				case "full-200":
					w.Write(data)
					return
				case "auth-failure":
					w.WriteHeader(http.StatusUnauthorized)
					w.Write([]byte("error-page"))
					return
				}
				ranges := map[string]string{"wrong-offset": "bytes 0-9/10", "changed-total": "bytes 3-9/11", "wrong-length": "bytes 3-9/10"}
				w.Header().Set("Content-Range", ranges[mode])
				w.Header().Set("Content-Length", "10")
				body := data
				if mode == "changed-total" {
					w.Header().Set("Content-Length", "7")
					body = data[3:]
				}
				w.WriteHeader(http.StatusPartialContent)
				w.Write(body)
			}))
			defer upstream.Close()
			req := blobRequest(t, upstream.URL, data)
			resp, cancel, err := openRegistryBlob(upstream.Client(), req, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			var dst bytes.Buffer
			result, err := streamRegistryBlob(req.Context(), &dst, upstream.Client(), req, resp, cancel, time.Second)
			if err == nil || result.failure != "resume_rejected" || dst.String() != "012" || calls.Load() != 2 {
				t.Fatalf("unsafe retry = %+v, err=%v, body=%q, calls=%d", result, err, dst.String(), calls.Load())
			}
		})
	}
}

func TestRegistryBlobRetriesAreBounded(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		w.Header().Set("Content-Length", strconv.Itoa(11-n))
		if n > 1 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-9/10", n-1))
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Write([]byte("x"))
	}))
	defer upstream.Close()
	req := blobRequest(t, upstream.URL, []byte("xxxxxxxxxx"))
	resp, cancel, err := openRegistryBlob(upstream.Client(), req, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	var dst bytes.Buffer
	result, err := streamRegistryBlob(req.Context(), &dst, upstream.Client(), req, resp, cancel, time.Second)
	if !errors.Is(err, io.ErrUnexpectedEOF) || result.resumes != blobMaxResumes || calls.Load() != 4 || dst.Len() != 4 {
		t.Fatalf("bounded retry = %+v, err=%v, calls=%d, bytes=%d", result, err, calls.Load(), dst.Len())
	}
}

type closedBlobWriter struct{}

func (closedBlobWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRegistryBlobNeverRetriesDisconnectedClients(t *testing.T) {
	for _, mode := range []string{"writer-closed", "context-canceled"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Length", "10")
				w.Write([]byte("0123456789"))
			}))
			defer upstream.Close()
			ctx, clientCancel := context.WithCancel(context.Background())
			defer clientCancel()
			req := blobRequest(t, upstream.URL, []byte("0123456789")).WithContext(ctx)
			resp, cancel, err := openRegistryBlob(upstream.Client(), req, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			var dst io.Writer = closedBlobWriter{}
			if mode == "context-canceled" {
				clientCancel()
				dst = io.Discard
			}
			result, err := streamRegistryBlob(ctx, dst, upstream.Client(), req, resp, cancel, time.Second)
			if err == nil || result.resumes != 0 || calls.Load() != 1 {
				t.Fatalf("client failure retried: %+v, err=%v, calls=%d", result, err, calls.Load())
			}
		})
	}
}

func TestRegistryBlobHandlerPreservesHeadersAndPrivateAuth(t *testing.T) {
	data := bytes.Repeat([]byte("registry-layer"), 4096)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Query().Get("ns") != "" || r.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Error("registry request lost auth or forwarded the local ns selector")
		}
		w.Header().Set("ETag", `"immutable"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		if n == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data[:4096])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)-4096))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 4096-%d/%d", len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[4096:])
	}))
	defer upstream.Close()
	withRegistryEndpoint(t, registryEndpoint{Domain: "ghcr.io", Upstream: upstream.URL, AuthType: "github"})
	loadDockerHandlerTestConfig(t, "")
	req := blobRequest(t, "http://proxy.example", data)
	w := performDockerHandlerRequest(newDockerHandlerTestRouter(), http.MethodGet, req.URL.Path+"?ns=ghcr.io", req.Header.Get("Authorization"))
	if w.Code != http.StatusOK || w.Header().Get("Content-Length") != strconv.Itoa(len(data)) ||
		w.Header().Get("Content-Range") != "" || w.Header().Get("ETag") != `"immutable"` || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("proxy response changed during resume: status=%d headers=%v bytes=%d", w.Code, w.Header(), w.Body.Len())
	}
	if calls.Load() != 2 || strings.Contains(w.Body.String(), "Internal server error") {
		t.Fatal("invalid stream completion")
	}
}

func TestRegistryBlobFailureBeforeAnyBytesReturnsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"not-transferred"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	withRegistryEndpoint(t, registryEndpoint{Domain: "ghcr.io", Upstream: upstream.URL, AuthType: "github"})
	loadDockerHandlerTestConfig(t, "")
	req := blobRequest(t, "http://proxy.example", []byte("0123456789"))
	w := performDockerHandlerRequest(newDockerHandlerTestRouter(), http.MethodGet, req.URL.Path+"?ns=ghcr.io", req.Header.Get("Authorization"))
	if w.Code != http.StatusBadGateway || w.Header().Get("ETag") != "" ||
		w.Body.String() != "Upstream registry request failed" {
		t.Fatalf("empty failed blob looked successful: status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
}
