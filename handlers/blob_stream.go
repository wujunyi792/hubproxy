package handlers

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	blobReadTimeout = 45 * time.Second
	blobMaxResumes  = 3
)

// Resume only immutable blob GETs, never manifests, tags, or request bodies.
func isRegistryBlobGET(req *http.Request) bool {
	_, digest, found := strings.Cut(req.URL.Path, "/blobs/sha256:")
	if req.Method != http.MethodGet || !found || len(digest) != 64 ||
		(req.Body != nil && req.Body != http.NoBody) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// Each attempt owns its cancellation. Time only upstream I/O, not a slow
// downstream writer; restarting an expired CDN redirect uses the registry URL.
func openRegistryBlob(client *http.Client, req *http.Request, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(req.Context())
	timer := time.AfterFunc(timeout, cancel)
	resp, err := client.Do(req.Clone(ctx))
	expired := !timer.Stop()
	if err != nil {
		cancel()
		if expired {
			err = context.DeadlineExceeded
		}
		return nil, nil, err
	}
	return resp, cancel, nil
}

type blobSpan struct{ start, end, total int64 }

func responseBlobSpan(resp *http.Response) (blobSpan, bool) {
	if resp.Uncompressed || (resp.Header.Get("Content-Encoding") != "" &&
		resp.Header.Get("Content-Encoding") != "identity") {
		return blobSpan{}, false
	}
	if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
		return blobSpan{0, resp.ContentLength - 1, resp.ContentLength}, true
	}
	if resp.StatusCode != http.StatusPartialContent {
		return blobSpan{}, false
	}
	value := strings.TrimPrefix(resp.Header.Get("Content-Range"), "bytes ")
	rangeText, totalText, ok := strings.Cut(value, "/")
	if !ok || value == resp.Header.Get("Content-Range") {
		return blobSpan{}, false
	}
	startText, endText, ok := strings.Cut(rangeText, "-")
	if !ok {
		return blobSpan{}, false
	}
	start, a := strconv.ParseInt(startText, 10, 64)
	end, b := strconv.ParseInt(endText, 10, 64)
	total, c := strconv.ParseInt(totalText, 10, 64)
	if a != nil || b != nil || c != nil || start < 0 || end < start || total <= end ||
		(resp.ContentLength >= 0 && resp.ContentLength != end-start+1) {
		return blobSpan{}, false
	}
	return blobSpan{start, end, total}, true
}

type blobStreamResult struct {
	bytes, expected int64
	resumes         int
	failure         string
}

// Return the failure direction separately: only upstream reads may be retried.
func copyBlobAttempt(ctx context.Context, dst io.Writer, src io.Reader, cancel context.CancelFunc,
	remaining int64, timeout time.Duration) (written int64, side string, err error) {
	buf := make([]byte, 128*1024)
	for remaining < 0 || written < remaining {
		if err := ctx.Err(); err != nil {
			return written, "client_canceled", err
		}
		chunk := buf
		if remaining >= 0 && remaining-written < int64(len(chunk)) {
			chunk = chunk[:remaining-written]
		}
		timer := time.AfterFunc(timeout, cancel)
		n, readErr := src.Read(chunk)
		if !timer.Stop() && readErr != nil {
			readErr = context.DeadlineExceeded
		}
		if n > 0 {
			count, writeErr := dst.Write(chunk[:n])
			written += int64(count)
			if writeErr == nil && count != n {
				writeErr = io.ErrShortWrite
			}
			if writeErr != nil {
				return written, "downstream_write", writeErr
			}
		}
		if remaining >= 0 && written == remaining {
			return written, "", nil
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if remaining < 0 {
					return written, "", nil
				}
				readErr = io.ErrUnexpectedEOF
			}
			return written, "upstream_read", readErr
		}
	}
	return written, "", nil
}

func streamRegistryBlob(ctx context.Context, dst io.Writer, client *http.Client, req *http.Request,
	resp *http.Response, cancel context.CancelFunc, timeout time.Duration) (result blobStreamResult, err error) {
	span, resumable := responseBlobSpan(resp)
	resumable = resumable && isRegistryBlobGET(req)
	result.expected = resp.ContentLength
	if resumable {
		result.expected = span.end - span.start + 1
	}
	for {
		remaining := result.expected
		if remaining >= 0 {
			remaining -= result.bytes
		}
		var n int64
		n, result.failure, err = copyBlobAttempt(ctx, dst, resp.Body, cancel, remaining, timeout)
		result.bytes += n
		resp.Body.Close()
		cancel()
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			result.failure = "client_canceled"
			return result, ctx.Err()
		}
		if !resumable || result.failure != "upstream_read" || result.resumes == blobMaxResumes {
			return result, err
		}
		for {
			if result.resumes == blobMaxResumes {
				return result, err
			}
			// Bounded backoff, interrupted immediately if the client leaves.
			result.resumes++
			select {
			case <-ctx.Done():
				result.failure = "client_canceled"
				return result, ctx.Err()
			case <-time.After(time.Duration(result.resumes) * 200 * time.Millisecond):
			}
			next := req.Clone(ctx)
			next.Header.Set("Range", "bytes="+strconv.FormatInt(span.start+result.bytes, 10)+"-"+strconv.FormatInt(span.end, 10))
			next.Header.Set("Accept-Encoding", "identity")
			resp, cancel, err = openRegistryBlob(client, next, timeout)
			if err != nil {
				result.failure = "upstream_request"
				continue
			}
			got, ok := responseBlobSpan(resp)
			// Never append a full 200 response, a different range, or an error page.
			if !ok || resp.StatusCode != http.StatusPartialContent ||
				got.start != span.start+result.bytes || got.end != span.end || got.total != span.total {
				resp.Body.Close()
				cancel()
				result.failure = "resume_rejected"
				return result, errors.New("upstream did not honor the blob resume range")
			}
			break
		}
	}
}

// Fixed categories keep signed CDN URLs and credentials out of failure logs.
func blobErrorKind(err error) string {
	var netErr net.Error
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "io_error"
	}
}
