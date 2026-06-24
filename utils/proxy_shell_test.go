package utils

import (
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

func TestProcessSmartRewritesGitHubURLs(t *testing.T) {
	input := `curl -L https://github.com/user/repo/releases/download/v1/file.sh`
	reader, size, err := ProcessSmart(strings.NewReader(input), false, "proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}

	buf := new(strings.Builder)
	if _, err := io.Copy(buf, reader); err != nil {
		t.Fatal(err)
	}

	want := "https://proxy.example.com/https://github.com/user/repo/releases/download/v1/file.sh"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("processed script = %q, want contains %q", buf.String(), want)
	}
	if size != int64(len(buf.String())) {
		t.Fatalf("size = %d, want %d", size, len(buf.String()))
	}
}

func TestProcessSmartKeepsNonGitHubContent(t *testing.T) {
	input := "echo hello"
	reader, _, err := ProcessSmart(strings.NewReader(input), false, "proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}

	buf := new(strings.Builder)
	if _, err := io.Copy(buf, reader); err != nil {
		t.Fatal(err)
	}
	if buf.String() != input {
		t.Fatalf("content changed: %q", buf.String())
	}
}

func TestReadShellContentGzip(t *testing.T) {
	var compressed strings.Builder
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write([]byte("echo https://github.com/u/r")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	reader, _, err := ProcessSmart(strings.NewReader(compressed.String()), true, "proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}

	buf := new(strings.Builder)
	if _, err := io.Copy(buf, reader); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "https://proxy.example.com/https://github.com/u/r") {
		t.Fatalf("gzip content not rewritten: %q", buf.String())
	}
}
