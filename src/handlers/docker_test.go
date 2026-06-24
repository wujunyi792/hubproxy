package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
	"hubproxy/utils"
)

func TestParseRegistryPath(t *testing.T) {
	tests := []struct {
		path      string
		image     string
		apiType   string
		reference string
	}{
		{"library/nginx/manifests/latest", "library/nginx", "manifests", "latest"},
		{"library/nginx/blobs/sha256:abc", "library/nginx", "blobs", "sha256:abc"},
		{"library/nginx/tags/list", "library/nginx", "tags", "list"},
	}

	for _, tt := range tests {
		image, apiType, reference := parseRegistryPath(tt.path)
		if image != tt.image || apiType != tt.apiType || reference != tt.reference {
			t.Fatalf("parseRegistryPath(%q) = %q %q %q", tt.path, image, apiType, reference)
		}
	}
}

func TestParseRegistryPathInvalid(t *testing.T) {
	image, apiType, reference := parseRegistryPath("library/nginx/unknown/latest")
	if image != "" || apiType != "" || reference != "" {
		t.Fatalf("invalid path parsed as %q %q %q", image, apiType, reference)
	}
}

func TestRewriteAuthHeaderUsesProxyTokenRealm(t *testing.T) {
	header := `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:owner/app:pull"`

	got := rewriteAuthHeader(header, "https://proxy.example")

	if !strings.Contains(got, `realm="https://proxy.example/token"`) {
		t.Fatalf("rewritten header = %q", got)
	}
	if !strings.Contains(got, `service="ghcr.io"`) || !strings.Contains(got, `scope="repository:owner/app:pull"`) {
		t.Fatalf("rewritten header lost challenge parameters: %q", got)
	}
}

func TestRewriteAuthHeaderOnlyRewritesBearerChallenge(t *testing.T) {
	header := `Basic realm="https://basic.example", Bearer realm="https://ghcr.io/token",service="ghcr.io"`

	got := rewriteAuthHeader(header, "https://proxy.example")

	if !strings.Contains(got, `Basic realm="https://basic.example"`) {
		t.Fatalf("basic challenge was changed: %q", got)
	}
	if !strings.Contains(got, `Bearer realm="https://proxy.example/token"`) {
		t.Fatalf("bearer challenge was not rewritten: %q", got)
	}
}

func TestContainerdMirrorPrivateGHCRAuthFlow(t *testing.T) {
	var sawAnonymousManifest bool
	var sawBearerManifest bool
	var sawTokenBasicAuth bool
	var sawBearerBlob bool

	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/owner/app/manifests/tag":
			if r.URL.Query().Get("ns") != "" {
				t.Errorf("upstream request leaked containerd ns query: %s", r.URL.RawQuery)
			}
			if got := r.Header.Get("X-Client-Hop"); got != "" {
				t.Errorf("upstream received hop-by-hop request header X-Client-Hop=%q", got)
			}
			switch auth := r.Header.Get("Authorization"); auth {
			case "":
				sawAnonymousManifest = true
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ghcr.io",scope="repository:owner/app:pull"`, upstream.URL))
				w.WriteHeader(http.StatusUnauthorized)
			case "Bearer upstream-token":
				sawBearerManifest = true
				w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
				w.Header().Set("Docker-Content-Digest", "sha256:abc")
				w.Header().Set("Connection", "X-Upstream-Hop")
				w.Header().Set("X-Upstream-Hop", "drop-me")
				w.WriteHeader(http.StatusOK)
			default:
				t.Errorf("unexpected upstream Authorization header: %q", auth)
				w.WriteHeader(http.StatusForbidden)
			}
		case "/v2/owner/app/blobs/sha256:abc":
			if r.URL.Query().Get("ns") != "" {
				t.Errorf("upstream blob request leaked containerd ns query: %s", r.URL.RawQuery)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer upstream-token" {
				t.Errorf("blob request Authorization = %q, want Bearer upstream-token", got)
			}
			sawBearerBlob = true
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", "10")
			w.Header().Set("Docker-Content-Digest", "sha256:abc")
			_, _ = w.Write([]byte("blob-bytes"))
		case "/token":
			if got := r.Header.Get("Authorization"); got == "Basic user-pat" {
				sawTokenBasicAuth = true
			} else if got == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			} else {
				t.Errorf("token request Authorization = %q, want Basic user-pat", got)
			}
			if got := r.URL.Query().Get("service"); got != "ghcr.io" {
				t.Errorf("token request service = %q, want ghcr.io", got)
			}
			if got := r.URL.Query().Get("scope"); got != "repository:owner/app:pull" {
				t.Errorf("token request scope = %q, want repository:owner/app:pull", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"upstream-token"}`))
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	loadDockerHandlerTestConfig(t, fmt.Sprintf(`
[registries."ghcr.io"]
upstream = %q
authHost = %q
authType = "github"
enabled = true
`, upstream.URL, upstream.URL+"/token"))

	router := newDockerHandlerTestRouter()

	w := performDockerHandlerRequest(router, http.MethodHead, "/v2/owner/app/manifests/tag?ns=ghcr.io", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous manifest status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	authHeader := w.Header().Get("WWW-Authenticate")
	if !strings.Contains(authHeader, `realm="https://proxy.example/token"`) {
		t.Fatalf("WWW-Authenticate = %q, want proxy token realm", authHeader)
	}

	w = performDockerHandlerRequest(router, http.MethodGet, "/token?service=ghcr.io&scope=repository%3Aowner%2Fapp%3Apull", "Basic user-pat")
	if w.Code != http.StatusOK {
		t.Fatalf("token status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) != `{"token":"upstream-token"}` {
		t.Fatalf("token body = %q", w.Body.String())
	}

	w = performDockerHandlerRequestWithHeaders(router, http.MethodHead, "/v2/owner/app/manifests/tag?ns=ghcr.io", map[string]string{
		"Authorization":  "Bearer upstream-token",
		"Connection":     "X-Client-Hop",
		"X-Client-Hop":   "drop-me",
		"X-Regular-Test": "keep-me",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("bearer manifest status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Docker-Content-Digest"); got != "sha256:abc" {
		t.Fatalf("Docker-Content-Digest = %q, want sha256:abc", got)
	}
	if got := w.Header().Get("X-Upstream-Hop"); got != "" {
		t.Fatalf("hop-by-hop response header X-Upstream-Hop = %q, want empty", got)
	}

	w = performDockerHandlerRequest(router, http.MethodGet, "/v2/owner/app/blobs/sha256:abc?ns=ghcr.io", "Bearer upstream-token")
	if w.Code != http.StatusOK {
		t.Fatalf("blob status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "blob-bytes" {
		t.Fatalf("blob body = %q, want blob-bytes", got)
	}

	w = performDockerHandlerRequest(router, http.MethodGet, "/token?service=ghcr.io&scope=repository%3Aowner%2Fapp%3Apull", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous token status after credentialed token = %d, want 401; body=%s", w.Code, w.Body.String())
	}

	if !sawAnonymousManifest || !sawTokenBasicAuth || !sawBearerManifest || !sawBearerBlob {
		t.Fatalf("flow incomplete: anonymous=%v tokenBasic=%v bearer=%v blob=%v", sawAnonymousManifest, sawTokenBasicAuth, sawBearerManifest, sawBearerBlob)
	}
}

func TestAuthenticatedTokenRequestFailsClosedForUnknownService(t *testing.T) {
	loadDockerHandlerTestConfig(t, ``)
	router := newDockerHandlerTestRouter()

	w := performDockerHandlerRequest(router, http.MethodGet, "/token?service=custom.example&scope=repository%3Aowner%2Fapp%3Apull", "Basic private-creds")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unsupported auth service") {
		t.Fatalf("unexpected body: %q", w.Body.String())
	}
}

func TestRequestSchemeUsesFirstForwardedProto(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("Forwarded", `for=client;proto=https, for=proxy;proto=http`)

	if got := requestScheme(c); got != "https" {
		t.Fatalf("requestScheme = %q, want https", got)
	}
}

func loadDockerHandlerTestConfig(t *testing.T, body string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	utils.InitHTTPClients()
}

func newDockerHandlerTestRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Any("/token", ProxyDockerAuthGin)
	router.Any("/token/*path", ProxyDockerAuthGin)
	router.Any("/v2/*path", ProxyDockerRegistryGin)
	return router
}

func performDockerHandlerRequest(router http.Handler, method, target, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = "proxy.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("User-Agent", "containerd/v2.2.0")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func performDockerHandlerRequestWithHeaders(router http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = "proxy.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("User-Agent", "containerd/v2.2.0")
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}
