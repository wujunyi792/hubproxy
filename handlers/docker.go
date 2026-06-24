package handlers

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"hubproxy/config"
	"hubproxy/utils"
)

// DockerProxy Docker代理配置
type DockerProxy struct {
	registry name.Registry
	options  []remote.Option
}

var dockerProxy *DockerProxy

type registryEndpoint struct {
	Domain       string
	Upstream     string
	AuthHost     string
	AuthType     string
	HostAliases  []string
	AuthServices []string
}

var registryEndpoints = map[string]registryEndpoint{
	"docker.io": {
		Domain:       "docker.io",
		Upstream:     "registry-1.docker.io",
		AuthHost:     "auth.docker.io/token",
		AuthType:     "docker",
		HostAliases:  []string{"registry-1.docker.io", "index.docker.io"},
		AuthServices: []string{"registry.docker.io", "auth.docker.io", "index.docker.io", "docker.io"},
	},
	"docker.elastic.co": {
		Domain:   "docker.elastic.co",
		Upstream: "docker.elastic.co",
		AuthHost: "docker-auth.elastic.co/auth",
		AuthType: "elastic",
	},
	"gcr.io": {
		Domain:       "gcr.io",
		Upstream:     "gcr.io",
		AuthHost:     "gcr.io/v2/token",
		AuthType:     "google",
		AuthServices: []string{"gcr.io"},
	},
	"ghcr.io": {
		Domain:       "ghcr.io",
		Upstream:     "ghcr.io",
		AuthHost:     "ghcr.io/token",
		AuthType:     "github",
		AuthServices: []string{"ghcr.io"},
	},
	"mcr.microsoft.com": {
		Domain:   "mcr.microsoft.com",
		Upstream: "mcr.microsoft.com",
		AuthType: "anonymous",
	},
	"nvcr.io": {
		Domain:   "nvcr.io",
		Upstream: "nvcr.io",
		AuthHost: "nvcr.io/proxy_auth",
		AuthType: "nvcr",
	},
	"quay.io": {
		Domain:       "quay.io",
		Upstream:     "quay.io",
		AuthHost:     "quay.io/v2/auth",
		AuthType:     "quay",
		AuthServices: []string{"quay.io"},
	},
	"registry.k8s.io": {
		Domain:   "registry.k8s.io",
		Upstream: "registry.k8s.io",
		AuthType: "anonymous",
	},
}

// RegistryDetector Registry检测器
type RegistryDetector struct{}

type detectedRegistryRoute struct {
	domain        string
	remainingPath string
	known         bool
	enabled       bool
}

// detectRegistryRoute 检测Registry域名并返回路由状态
func (rd *RegistryDetector) detectRegistryRoute(c *gin.Context, path string) detectedRegistryRoute {
	// 兼容Containerd的ns参数
	if ns := c.Query("ns"); ns != "" {
		if domain, exists := findRegistryByHost(ns); exists {
			return detectedRegistryRoute{
				domain:        domain,
				remainingPath: path,
				known:         true,
				enabled:       rd.isRegistryEnabled(domain),
			}
		}
	}

	for _, domain := range supportedRegistryDomains() {
		for _, prefix := range registryPathPrefixes(registryEndpoints[domain]) {
			if strings.HasPrefix(path, prefix+"/") {
				return detectedRegistryRoute{
					domain:        domain,
					remainingPath: strings.TrimPrefix(path, prefix+"/"),
					known:         true,
					enabled:       rd.isRegistryEnabled(domain),
				}
			}
		}
	}

	return detectedRegistryRoute{remainingPath: path}
}

// isRegistryEnabled 检查Registry是否启用
func (rd *RegistryDetector) isRegistryEnabled(domain string) bool {
	canonicalDomain, exists := findRegistryByHost(domain)
	if !exists {
		return false
	}

	cfg := config.GetConfig()
	if mapping, exists := cfg.Registries[canonicalDomain]; exists {
		return mapping.Enabled
	}
	return true
}

func (rd *RegistryDetector) getRegistryEndpoint(domain string) (registryEndpoint, bool) {
	canonicalDomain, exists := findRegistryByHost(domain)
	if !exists || !rd.isRegistryEnabled(canonicalDomain) {
		return registryEndpoint{}, false
	}
	return registryEndpoints[canonicalDomain], true
}

var registryDetector = &RegistryDetector{}

func supportedRegistryDomains() []string {
	domains := make([]string, 0, len(registryEndpoints))
	for domain := range registryEndpoints {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	return domains
}

func registryPathPrefixes(endpoint registryEndpoint) []string {
	prefixes := []string{endpoint.Domain}
	prefixes = append(prefixes, endpoint.HostAliases...)
	return prefixes
}

func findRegistryByHost(candidate string) (string, bool) {
	host := normalizeRegistryHost(candidate)
	if host == "" {
		return "", false
	}

	for _, domain := range supportedRegistryDomains() {
		endpoint := registryEndpoints[domain]
		if host == endpoint.Domain || host == normalizeRegistryHost(endpoint.Upstream) {
			return domain, true
		}
		for _, alias := range endpoint.HostAliases {
			if host == normalizeRegistryHost(alias) {
				return domain, true
			}
		}
	}

	return "", false
}

func findRegistryByAuthCandidate(candidate string) (registryEndpoint, bool) {
	host := normalizeRegistryHost(candidate)
	if host == "" {
		return registryEndpoint{}, false
	}

	for _, domain := range supportedRegistryDomains() {
		endpoint := registryEndpoints[domain]
		if host == endpoint.Domain || host == normalizeRegistryHost(endpoint.Upstream) || host == normalizeRegistryHost(endpoint.AuthHost) {
			return endpoint, true
		}
		for _, alias := range endpoint.HostAliases {
			if host == normalizeRegistryHost(alias) {
				return endpoint, true
			}
		}
		for _, service := range endpoint.AuthServices {
			if host == normalizeRegistryHost(service) {
				return endpoint, true
			}
		}
	}

	return registryEndpoint{}, false
}

func logDockerProxy(c *gin.Context, format string, args ...interface{}) {
	logArgs := append([]interface{}{
		c.ClientIP(),
		c.Request.Method,
		c.Request.Host,
		c.Request.URL.Path,
		c.Request.URL.RawQuery,
	}, args...)
	fmt.Printf(
		"[docker-proxy] client=%s method=%s host=%s path=%s query=%s "+format+"\n",
		logArgs...,
	)
}

func hasAuthorization(c *gin.Context) bool {
	return strings.TrimSpace(c.GetHeader("Authorization")) != ""
}

// InitDockerProxy 初始化Docker代理
func InitDockerProxy() {
	registry, err := name.NewRegistry("registry-1.docker.io")
	if err != nil {
		fmt.Printf("创建Docker registry失败: %v\n", err)
		return
	}

	options := []remote.Option{
		remote.WithAuth(authn.Anonymous),
		remote.WithUserAgent("hubproxy/go-containerregistry"),
		remote.WithTransport(utils.GetGlobalHTTPClient().Transport),
	}

	dockerProxy = &DockerProxy{
		registry: registry,
		options:  options,
	}
}

// ProxyDockerRegistryGin 标准Docker Registry API v2代理
func ProxyDockerRegistryGin(c *gin.Context) {
	path := c.Request.URL.Path

	if path == "/v2/" {
		logDockerProxy(c, "event=v2_ping status=200")
		c.JSON(http.StatusOK, gin.H{})
		return
	}

	if strings.HasPrefix(path, "/v2/") {
		handleRegistryRequest(c, path)
	} else {
		logDockerProxy(c, "event=non_registry_path status=404")
		c.String(http.StatusNotFound, "Docker Registry API v2 only")
	}
}

// handleRegistryRequest 处理Registry请求
func handleRegistryRequest(c *gin.Context, path string) {
	pathWithoutV2 := strings.TrimPrefix(path, "/v2/")

	route := registryDetector.detectRegistryRoute(c, pathWithoutV2)
	if route.known {
		if !route.enabled {
			logDockerProxy(c, "event=registry_disabled registry=%s remaining=%s source=%s status=403", route.domain, route.remainingPath, registryRouteSource(c, pathWithoutV2, route.domain))
			c.String(http.StatusForbidden, "Registry disabled")
			return
		}
		c.Set("target_registry_domain", route.domain)
		c.Set("target_path", route.remainingPath)
		logDockerProxy(c, "event=registry_route registry=%s remaining=%s source=%s", route.domain, route.remainingPath, registryRouteSource(c, pathWithoutV2, route.domain))

		handleMultiRegistryRequest(c, route.domain, route.remainingPath)
		return
	}

	imageName, apiType, reference := parseRegistryPath(pathWithoutV2)
	if imageName == "" || apiType == "" {
		logDockerProxy(c, "event=invalid_registry_path raw_path=%s status=400", pathWithoutV2)
		c.String(http.StatusBadRequest, "Invalid path format")
		return
	}

	if !strings.Contains(imageName, "/") {
		imageName = "library/" + imageName
	}

	if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(imageName); !allowed {
		fmt.Printf("Docker镜像 %s 访问被拒绝: %s\n", imageName, reason)
		logDockerProxy(c, "event=access_denied image=%s reason=%s status=403", imageName, reason)
		c.String(http.StatusForbidden, "镜像访问被限制")
		return
	}

	imageRef := fmt.Sprintf("%s/%s", dockerProxy.registry.Name(), imageName)

	switch apiType {
	case "manifests":
		logDockerProxy(c, "event=dockerhub_manifest image=%s reference=%s has_auth=%t", imageRef, reference, hasAuthorization(c))
		handleManifestRequest(c, imageRef, reference)
	case "blobs":
		logDockerProxy(c, "event=dockerhub_blob image=%s digest=%s has_auth=%t", imageRef, reference, hasAuthorization(c))
		handleBlobRequest(c, imageRef, reference)
	case "tags":
		logDockerProxy(c, "event=dockerhub_tags image=%s has_auth=%t", imageRef, hasAuthorization(c))
		handleTagsRequest(c, imageRef)
	default:
		logDockerProxy(c, "event=unknown_api api=%s image=%s status=404", apiType, imageName)
		c.String(http.StatusNotFound, "API endpoint not found")
	}
}

func registryRouteSource(c *gin.Context, path, registryDomain string) string {
	if ns := c.Query("ns"); ns != "" {
		if domain, exists := findRegistryByHost(ns); exists && domain == registryDomain {
			return "containerd_ns"
		}
	}
	if endpoint, exists := registryEndpoints[registryDomain]; exists {
		for _, prefix := range registryPathPrefixes(endpoint) {
			if strings.HasPrefix(path, prefix+"/") {
				return "path_prefix"
			}
		}
	}
	return "unknown"
}

// parseRegistryPath 解析Registry路径
func parseRegistryPath(path string) (imageName, apiType, reference string) {
	if idx := strings.Index(path, "/manifests/"); idx != -1 {
		imageName = path[:idx]
		apiType = "manifests"
		reference = path[idx+len("/manifests/"):]
		return
	}

	if idx := strings.Index(path, "/blobs/"); idx != -1 {
		imageName = path[:idx]
		apiType = "blobs"
		reference = path[idx+len("/blobs/"):]
		return
	}

	if idx := strings.Index(path, "/tags/list"); idx != -1 {
		imageName = path[:idx]
		apiType = "tags"
		reference = "list"
		return
	}

	return "", "", ""
}

// handleManifestRequest 处理manifest请求
func handleManifestRequest(c *gin.Context, imageRef, reference string) {
	if utils.IsCacheEnabled() && c.Request.Method == http.MethodGet {
		cacheKey := utils.BuildManifestCacheKey(imageRef, reference)

		if cachedItem := utils.GlobalCache.Get(cacheKey); cachedItem != nil {
			utils.WriteCachedResponse(c, cachedItem)
			return
		}
	}

	var ref name.Reference
	var err error

	if strings.HasPrefix(reference, "sha256:") {
		ref, err = name.NewDigest(fmt.Sprintf("%s@%s", imageRef, reference))
	} else {
		ref, err = name.NewTag(fmt.Sprintf("%s:%s", imageRef, reference))
	}

	if err != nil {
		fmt.Printf("解析镜像引用失败: %v\n", err)
		logDockerProxy(c, "event=manifest_parse_error image=%s reference=%s error=%q status=400", imageRef, reference, err)
		c.String(http.StatusBadRequest, "Invalid reference")
		return
	}

	if c.Request.Method == http.MethodHead {
		desc, err := remote.Head(ref, dockerProxy.options...)
		if err != nil {
			fmt.Printf("HEAD请求失败: %v\n", err)
			logDockerProxy(c, "event=dockerhub_manifest_head_error image=%s reference=%s error=%q status=404", imageRef, reference, err)
			c.String(http.StatusNotFound, "Manifest not found")
			return
		}

		logDockerProxy(c, "event=dockerhub_manifest_head_ok image=%s reference=%s digest=%s media_type=%s size=%d status=200", imageRef, reference, desc.Digest.String(), desc.MediaType, desc.Size)
		c.Header("Content-Type", string(desc.MediaType))
		c.Header("Docker-Content-Digest", desc.Digest.String())
		c.Header("Content-Length", fmt.Sprintf("%d", desc.Size))
		c.Status(http.StatusOK)
	} else {
		desc, err := remote.Get(ref, dockerProxy.options...)
		if err != nil {
			fmt.Printf("GET请求失败: %v\n", err)
			logDockerProxy(c, "event=dockerhub_manifest_get_error image=%s reference=%s error=%q status=404", imageRef, reference, err)
			c.String(http.StatusNotFound, "Manifest not found")
			return
		}

		headers := map[string]string{
			"Docker-Content-Digest": desc.Digest.String(),
			"Content-Length":        fmt.Sprintf("%d", len(desc.Manifest)),
		}

		if utils.IsCacheEnabled() {
			cacheKey := utils.BuildManifestCacheKey(imageRef, reference)
			ttl := utils.GetManifestTTL(reference)
			utils.GlobalCache.Set(cacheKey, desc.Manifest, string(desc.MediaType), headers, ttl)
			logDockerProxy(c, "event=manifest_cache_store image=%s reference=%s ttl=%s", imageRef, reference, ttl)
		}

		logDockerProxy(c, "event=dockerhub_manifest_get_ok image=%s reference=%s digest=%s media_type=%s bytes=%d status=200", imageRef, reference, desc.Digest.String(), desc.MediaType, len(desc.Manifest))
		c.Header("Content-Type", string(desc.MediaType))
		for key, value := range headers {
			c.Header(key, value)
		}

		c.Data(http.StatusOK, string(desc.MediaType), desc.Manifest)
	}
}

// handleBlobRequest 处理blob请求
func handleBlobRequest(c *gin.Context, imageRef, digest string) {
	digestRef, err := name.NewDigest(fmt.Sprintf("%s@%s", imageRef, digest))
	if err != nil {
		fmt.Printf("解析digest引用失败: %v\n", err)
		logDockerProxy(c, "event=blob_parse_error image=%s digest=%s error=%q status=400", imageRef, digest, err)
		c.String(http.StatusBadRequest, "Invalid digest reference")
		return
	}

	layer, err := remote.Layer(digestRef, dockerProxy.options...)
	if err != nil {
		fmt.Printf("获取layer失败: %v\n", err)
		logDockerProxy(c, "event=dockerhub_blob_layer_error image=%s digest=%s error=%q status=404", imageRef, digest, err)
		c.String(http.StatusNotFound, "Layer not found")
		return
	}

	size, err := layer.Size()
	if err != nil {
		fmt.Printf("获取layer大小失败: %v\n", err)
		logDockerProxy(c, "event=dockerhub_blob_size_error image=%s digest=%s error=%q status=500", imageRef, digest, err)
		c.String(http.StatusInternalServerError, "Failed to get layer size")
		return
	}

	reader, err := layer.Compressed()
	if err != nil {
		fmt.Printf("获取layer内容失败: %v\n", err)
		logDockerProxy(c, "event=dockerhub_blob_reader_error image=%s digest=%s error=%q status=500", imageRef, digest, err)
		c.String(http.StatusInternalServerError, "Failed to get layer content")
		return
	}
	defer reader.Close()

	logDockerProxy(c, "event=dockerhub_blob_stream_start image=%s digest=%s size=%d status=200", imageRef, digest, size)
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", fmt.Sprintf("%d", size))
	c.Header("Docker-Content-Digest", digest)

	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		fmt.Printf("复制layer内容失败: %v\n", err)
		logDockerProxy(c, "event=dockerhub_blob_stream_error image=%s digest=%s error=%q", imageRef, digest, err)
	} else {
		logDockerProxy(c, "event=dockerhub_blob_stream_complete image=%s digest=%s size=%d", imageRef, digest, size)
	}
}

// handleTagsRequest 处理tags列表请求
func handleTagsRequest(c *gin.Context, imageRef string) {
	repo, err := name.NewRepository(imageRef)
	if err != nil {
		fmt.Printf("解析repository失败: %v\n", err)
		logDockerProxy(c, "event=tags_parse_error image=%s error=%q status=400", imageRef, err)
		c.String(http.StatusBadRequest, "Invalid repository")
		return
	}

	tags, err := remote.List(repo, dockerProxy.options...)
	if err != nil {
		fmt.Printf("获取tags失败: %v\n", err)
		logDockerProxy(c, "event=dockerhub_tags_error image=%s error=%q status=404", imageRef, err)
		c.String(http.StatusNotFound, "Tags not found")
		return
	}

	logDockerProxy(c, "event=dockerhub_tags_ok image=%s count=%d status=200", imageRef, len(tags))
	response := map[string]interface{}{
		"name": strings.TrimPrefix(imageRef, dockerProxy.registry.Name()+"/"),
		"tags": tags,
	}

	c.JSON(http.StatusOK, response)
}

// ProxyDockerAuthGin Docker认证代理
func ProxyDockerAuthGin(c *gin.Context) {
	logDockerProxy(c, "event=auth_proxy_start service=%s scope=%s has_auth=%t cache_enabled=%t", c.Query("service"), c.Query("scope"), hasAuthorization(c), utils.IsTokenCacheEnabled())
	if utils.IsTokenCacheEnabled() {
		proxyDockerAuthWithCache(c)
	} else {
		proxyDockerAuthOriginal(c)
	}
}

// proxyDockerAuthWithCache 带缓存的认证代理
func proxyDockerAuthWithCache(c *gin.Context) {
	if hasAuthorization(c) {
		logDockerProxy(c, "event=token_cache_bypass reason=credentialed_request service=%s scope=%s", c.Query("service"), c.Query("scope"))
		proxyDockerAuthOriginal(c)
		return
	}

	cacheKey := utils.BuildTokenCacheKey(c.Request.URL.RawQuery)

	if cachedToken := utils.GlobalCache.GetToken(cacheKey); cachedToken != "" {
		logDockerProxy(c, "event=token_cache_hit key=%s service=%s scope=%s", cacheKey, c.Query("service"), c.Query("scope"))
		utils.WriteTokenResponse(c, cachedToken)
		return
	}
	logDockerProxy(c, "event=token_cache_miss key=%s service=%s scope=%s", cacheKey, c.Query("service"), c.Query("scope"))

	recorder := &ResponseRecorder{
		ResponseWriter: c.Writer,
		statusCode:     200,
	}
	c.Writer = recorder

	proxyDockerAuthOriginal(c)

	if recorder.statusCode == 200 && len(recorder.body) > 0 {
		ttl := utils.ExtractTTLFromResponse(recorder.body)
		utils.GlobalCache.SetToken(cacheKey, string(recorder.body), ttl)
		logDockerProxy(c, "event=token_cache_store key=%s ttl=%s service=%s scope=%s", cacheKey, ttl, c.Query("service"), c.Query("scope"))
	}

	c.Writer = recorder.ResponseWriter
	c.Data(recorder.statusCode, "application/json", recorder.body)
}

// ResponseRecorder HTTP响应记录器
type ResponseRecorder struct {
	gin.ResponseWriter
	statusCode int
	body       []byte
}

func (r *ResponseRecorder) WriteHeader(code int) {
	r.statusCode = code
}

func (r *ResponseRecorder) Write(data []byte) (int, error) {
	r.body = append(r.body, data...)
	return len(data), nil
}

func proxyDockerAuthOriginal(c *gin.Context) {
	authURL, err := buildDockerAuthURL(c)
	if err != nil {
		logDockerProxy(c, "event=auth_proxy_reject service=%s scope=%s has_auth=%t error=%q status=400", c.Query("service"), c.Query("scope"), hasAuthorization(c), err)
		c.String(http.StatusBadRequest, err.Error())
		return
	}

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: utils.GetGlobalHTTPClient().Transport,
	}

	req, err := http.NewRequestWithContext(
		c.Request.Context(),
		c.Request.Method,
		authURL,
		c.Request.Body,
	)
	if err != nil {
		logDockerProxy(c, "event=auth_request_create_error target=%s error=%q status=500", authURL, err)
		c.String(http.StatusInternalServerError, "Failed to create request")
		return
	}

	copyProxyRequestHeaders(req.Header, c.Request.Header)
	logDockerProxy(c, "event=auth_upstream_request target=%s service=%s scope=%s has_auth=%t", authURL, c.Query("service"), c.Query("scope"), hasAuthorization(c))

	resp, err := client.Do(req)
	if err != nil {
		logDockerProxy(c, "event=auth_upstream_error target=%s error=%q status=502", authURL, err)
		c.String(http.StatusBadGateway, "Auth request failed")
		return
	}
	defer resp.Body.Close()

	proxyBaseURL := getProxyBaseURL(c)
	authRegistryDomain := ""
	if endpoint, found := detectAuthRegistryEndpoint(c); found {
		authRegistryDomain = endpoint.Domain
	}

	connectionHeaders := connectionHeaderNames(resp.Header)
	for key, values := range resp.Header {
		for _, value := range values {
			if shouldSkipProxyHeader(key, connectionHeaders) {
				continue
			}
			if strings.EqualFold(key, "WWW-Authenticate") {
				value = rewriteAuthHeader(value, proxyBaseURL, authRegistryDomain)
			}
			c.Writer.Header().Add(key, value)
		}
	}

	logDockerProxy(c, "event=auth_upstream_response target=%s upstream_status=%d challenge=%t content_type=%s", authURL, resp.StatusCode, resp.Header.Get("WWW-Authenticate") != "", resp.Header.Get("Content-Type"))
	c.Status(resp.StatusCode)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		fmt.Printf("复制认证响应失败: %v\n", err)
		logDockerProxy(c, "event=auth_response_copy_error target=%s error=%q", authURL, err)
	}
}

func buildDockerAuthURL(c *gin.Context) (string, error) {
	if domain, disabled := detectDisabledAuthRegistry(c); disabled {
		return "", fmt.Errorf("registry %q is disabled", domain)
	}

	if endpoint, found := detectAuthRegistryEndpoint(c); found {
		logDockerProxy(c, "event=auth_mapping_selected registry=%s service=%s auth_host=%s upstream=%s auth_type=%s ns=%s", endpoint.Domain, c.Query("service"), endpoint.AuthHost, endpoint.Upstream, endpoint.AuthType, c.Query("ns"))
		return buildMappedAuthURL(c, endpoint)
	}

	service := strings.TrimSpace(c.Query("service"))
	if isDockerHubAuthService(service) {
		if !registryDetector.isRegistryEnabled("docker.io") {
			return "", fmt.Errorf("registry %q is disabled", "docker.io")
		}
	} else if hasAuthorization(c) {
		return "", fmt.Errorf("unsupported auth service %q", service)
	}

	authURL := "https://auth.docker.io" + c.Request.URL.Path
	if rawQuery := authRequestRawQuery(c); rawQuery != "" {
		authURL += "?" + rawQuery
	}
	return authURL, nil
}

func detectDisabledAuthRegistry(c *gin.Context) (string, bool) {
	if targetDomain, exists := c.Get("target_registry_domain"); exists {
		if domain, found := findRegistryByHost(targetDomain.(string)); found && !registryDetector.isRegistryEnabled(domain) {
			return domain, true
		}
	}

	if ns := strings.TrimSpace(c.Query("ns")); ns != "" {
		if domain, found := findRegistryByHost(ns); found && !registryDetector.isRegistryEnabled(domain) {
			return domain, true
		}
	}

	for _, candidate := range []string{c.Query("service"), c.Query("realm")} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if endpoint, found := findRegistryByAuthCandidate(candidate); found && !registryDetector.isRegistryEnabled(endpoint.Domain) {
			return endpoint.Domain, true
		}
	}

	return "", false
}

func detectAuthRegistryEndpoint(c *gin.Context) (registryEndpoint, bool) {
	if targetDomain, exists := c.Get("target_registry_domain"); exists {
		if endpoint, found := registryDetector.getRegistryEndpoint(targetDomain.(string)); found {
			return endpoint, true
		}
	}

	if ns := strings.TrimSpace(c.Query("ns")); ns != "" {
		if domain, found := findRegistryByHost(ns); found {
			if endpoint, enabled := registryDetector.getRegistryEndpoint(domain); enabled {
				return endpoint, true
			}
		}
	}

	for _, candidate := range []string{c.Query("service"), c.Query("realm")} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if endpoint, found := findRegistryByAuthCandidate(candidate); found {
			if enabledEndpoint, enabled := registryDetector.getRegistryEndpoint(endpoint.Domain); enabled {
				return enabledEndpoint, true
			}
		}
	}

	return registryEndpoint{}, false
}

func buildMappedAuthURL(c *gin.Context, endpoint registryEndpoint) (string, error) {
	rawAuthHost := strings.TrimSpace(endpoint.AuthHost)
	if rawAuthHost == "" {
		return "", fmt.Errorf("registry %q does not use a token auth endpoint", endpoint.Domain)
	}
	if !strings.Contains(rawAuthHost, "://") {
		rawAuthHost = "https://" + rawAuthHost
	}

	parsed, err := url.Parse(rawAuthHost)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid auth endpoint for registry %q", endpoint.Domain)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = c.Request.URL.Path
	}
	parsed.RawQuery = authRequestRawQuery(c)

	return parsed.String(), nil
}

func supportsTokenProxy(endpoint registryEndpoint) bool {
	return strings.TrimSpace(endpoint.AuthHost) != ""
}

func authRequestRawQuery(c *gin.Context) string {
	query := c.Request.URL.Query()
	query.Del("ns")
	return query.Encode()
}

func isDockerHubAuthService(service string) bool {
	endpoint := registryEndpoints["docker.io"]
	service = normalizeRegistryHost(service)
	for _, candidate := range append(endpoint.AuthServices, endpoint.Domain, endpoint.Upstream) {
		if service == normalizeRegistryHost(candidate) {
			return true
		}
	}
	return false
}

// rewriteAuthHeader 重写认证头
func rewriteAuthHeader(authHeader, proxyBaseURL, registryDomain string) string {
	bearerStart := findBearerChallengeStart(authHeader)
	if bearerStart == -1 {
		return authHeader
	}

	return authHeader[:bearerStart] + rewriteAuthRealm(authHeader[bearerStart:], proxyBaseURL, registryDomain)
}

func findBearerChallengeStart(authHeader string) int {
	inQuote := false
	for i := 0; i < len(authHeader); i++ {
		switch authHeader[i] {
		case '"':
			inQuote = !inQuote
		case ',':
			if inQuote {
				continue
			}
			next := strings.TrimLeft(authHeader[i+1:], " \t")
			if len(next) >= len("Bearer ") && strings.EqualFold(next[:len("Bearer")], "Bearer") && (next[len("Bearer")] == ' ' || next[len("Bearer")] == '\t') {
				return i + 1 + len(authHeader[i+1:]) - len(next)
			}
		}
	}

	trimmed := strings.TrimLeft(authHeader, " \t")
	if len(trimmed) >= len("Bearer ") && strings.EqualFold(trimmed[:len("Bearer")], "Bearer") && (trimmed[len("Bearer")] == ' ' || trimmed[len("Bearer")] == '\t') {
		return len(authHeader) - len(trimmed)
	}
	return -1
}

func rewriteAuthRealm(authHeader, proxyBaseURL, registryDomain string) string {
	realm := buildProxyTokenRealm(proxyBaseURL, registryDomain)
	lower := strings.ToLower(authHeader)
	idx := strings.Index(lower, "realm=")
	if idx == -1 {
		return authHeader
	}

	valueStart := idx + len("realm=")
	if valueStart >= len(authHeader) {
		return authHeader
	}

	if authHeader[valueStart] == '"' {
		valueEnd := strings.Index(authHeader[valueStart+1:], "\"")
		if valueEnd == -1 {
			return authHeader
		}
		valueEnd += valueStart + 1
		return authHeader[:valueStart+1] + realm + authHeader[valueEnd:]
	}

	valueEnd := strings.Index(authHeader[valueStart:], ",")
	if valueEnd == -1 {
		return authHeader[:valueStart] + `"` + realm + `"`
	}
	valueEnd += valueStart
	return authHeader[:valueStart] + `"` + realm + `"` + authHeader[valueEnd:]
}

func buildProxyTokenRealm(proxyBaseURL, registryDomain string) string {
	realm := strings.TrimRight(proxyBaseURL, "/") + "/token"
	if registryDomain == "" {
		return realm
	}

	parsed, err := url.Parse(realm)
	if err != nil {
		return realm + "?ns=" + url.QueryEscape(registryDomain)
	}
	query := parsed.Query()
	query.Set("ns", registryDomain)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func getProxyBaseURL(c *gin.Context) string {
	proxyHost := c.Request.Host
	if proxyHost == "" {
		cfg := config.GetConfig()
		proxyHost = fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
		if cfg.Server.Host == "0.0.0.0" {
			proxyHost = fmt.Sprintf("localhost:%d", cfg.Server.Port)
		}
	}

	return requestScheme(c) + "://" + proxyHost
}

func requestScheme(c *gin.Context) string {
	if proto := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")); proto != "" {
		if idx := strings.Index(proto, ","); idx != -1 {
			proto = strings.TrimSpace(proto[:idx])
		}
		if proto == "http" || proto == "https" {
			return proto
		}
	}

	if forwarded := c.GetHeader("Forwarded"); forwarded != "" {
		for _, element := range strings.Split(forwarded, ",") {
			for _, part := range strings.Split(element, ";") {
				part = strings.TrimSpace(part)
				key, value, ok := strings.Cut(part, "=")
				if !ok || !strings.EqualFold(key, "proto") {
					continue
				}
				value = strings.Trim(value, `"`)
				if value == "http" || value == "https" {
					return value
				}
			}
		}
	}

	if c.Request.TLS != nil {
		return "https"
	}
	return "http"
}

// handleMultiRegistryRequest 处理多Registry请求
func handleMultiRegistryRequest(c *gin.Context, registryDomain, remainingPath string) {
	endpoint, exists := registryDetector.getRegistryEndpoint(registryDomain)
	if !exists {
		logDockerProxy(c, "event=registry_endpoint_missing registry=%s status=400", registryDomain)
		c.String(http.StatusBadRequest, "Registry not configured")
		return
	}

	imageName, apiType, _ := parseRegistryPath(remainingPath)
	if imageName == "" || apiType == "" {
		logDockerProxy(c, "event=multi_registry_invalid_path registry=%s remaining=%s status=400", registryDomain, remainingPath)
		c.String(http.StatusBadRequest, "Invalid path format")
		return
	}

	fullImageName := registryDomain + "/" + imageName
	if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(fullImageName); !allowed {
		fmt.Printf("镜像 %s 访问被拒绝: %s\n", fullImageName, reason)
		logDockerProxy(c, "event=multi_registry_access_denied registry=%s image=%s reason=%s status=403", registryDomain, fullImageName, reason)
		c.String(http.StatusForbidden, "镜像访问被限制")
		return
	}

	switch apiType {
	case "manifests":
	case "blobs":
	case "tags":
	default:
		logDockerProxy(c, "event=multi_registry_unknown_api registry=%s api=%s image=%s status=404", registryDomain, apiType, fullImageName)
		c.String(http.StatusNotFound, "API endpoint not found")
		return
	}

	logDockerProxy(c, "event=multi_registry_proxy_start registry=%s upstream=%s auth_type=%s image=%s api=%s has_auth=%t", registryDomain, endpoint.Upstream, endpoint.AuthType, fullImageName, apiType, hasAuthorization(c))
	if err := proxyUpstreamRegistryRequest(c, endpoint, remainingPath); err != nil {
		fmt.Printf("代理上游Registry失败: %v\n", err)
		logDockerProxy(c, "event=multi_registry_proxy_error registry=%s image=%s api=%s error=%q status=502", registryDomain, fullImageName, apiType, err)
		c.String(http.StatusBadGateway, "Upstream registry request failed")
		return
	}
}

func proxyUpstreamRegistryRequest(c *gin.Context, endpoint registryEndpoint, remainingPath string) error {
	upstreamURL, err := buildUpstreamRegistryURL(c, endpoint, remainingPath)
	if err != nil {
		return err
	}
	nsWasPresent := c.Request.URL.Query().Get("ns") != ""

	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstreamURL, c.Request.Body)
	if err != nil {
		return err
	}
	copyProxyRequestHeaders(req.Header, c.Request.Header)
	logDockerProxy(c, "event=upstream_registry_request target=%s ns_stripped=%t has_auth=%t accept=%q", upstreamURL, nsWasPresent, hasAuthorization(c), c.GetHeader("Accept"))

	resp, err := utils.GetGlobalHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	logDockerProxy(c, "event=upstream_registry_response target=%s upstream_status=%d challenge=%t digest=%s content_type=%s content_length=%s", upstreamURL, resp.StatusCode, resp.Header.Get("WWW-Authenticate") != "", resp.Header.Get("Docker-Content-Digest"), resp.Header.Get("Content-Type"), resp.Header.Get("Content-Length"))

	proxyBaseURL := getProxyBaseURL(c)
	connectionHeaders := connectionHeaderNames(resp.Header)
	for key, values := range resp.Header {
		if shouldSkipProxyHeader(key, connectionHeaders) {
			continue
		}
		for _, value := range values {
			if strings.EqualFold(key, "WWW-Authenticate") && supportsTokenProxy(endpoint) {
				value = rewriteAuthHeader(value, proxyBaseURL, endpoint.Domain)
			}
			c.Writer.Header().Add(key, value)
		}
	}

	c.Status(resp.StatusCode)
	if c.Request.Method == http.MethodHead {
		logDockerProxy(c, "event=upstream_registry_head_complete target=%s status=%d", upstreamURL, resp.StatusCode)
		return nil
	}

	if _, err = io.Copy(c.Writer, resp.Body); err != nil {
		fmt.Printf("复制上游Registry响应失败: %v\n", err)
		logDockerProxy(c, "event=upstream_registry_stream_error target=%s status=%d error=%q", upstreamURL, resp.StatusCode, err)
	} else {
		logDockerProxy(c, "event=upstream_registry_stream_complete target=%s status=%d", upstreamURL, resp.StatusCode)
	}
	return nil
}

func buildUpstreamRegistryURL(c *gin.Context, endpoint registryEndpoint, remainingPath string) (string, error) {
	rawUpstream := strings.TrimSpace(endpoint.Upstream)
	if rawUpstream == "" {
		return "", fmt.Errorf("empty upstream registry")
	}
	if !strings.Contains(rawUpstream, "://") {
		rawUpstream = "https://" + rawUpstream
	}

	parsed, err := url.Parse(rawUpstream)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("invalid upstream registry %q", endpoint.Upstream)
	}

	basePath := strings.TrimRight(parsed.Path, "/")
	parsed.Path = basePath + "/v2/" + strings.TrimLeft(remainingPath, "/")
	query := c.Request.URL.Query()
	query.Del("ns")
	parsed.RawQuery = query.Encode()

	return parsed.String(), nil
}

func copyProxyRequestHeaders(dst, src http.Header) {
	connectionHeaders := connectionHeaderNames(src)
	for key, values := range src {
		if shouldSkipProxyHeader(key, connectionHeaders) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func shouldSkipProxyHeader(header string, connectionHeaders map[string]struct{}) bool {
	if isHopByHopHeader(header) {
		return true
	}
	_, ok := connectionHeaders[strings.ToLower(header)]
	return ok
}

func isHopByHopHeader(header string) bool {
	switch strings.ToLower(header) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func connectionHeaderNames(headers http.Header) map[string]struct{} {
	names := make(map[string]struct{})
	for _, value := range headers.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" {
				names[name] = struct{}{}
			}
		}
	}
	return names
}

func normalizeRegistryHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Host
}
