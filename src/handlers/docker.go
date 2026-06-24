package handlers

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// RegistryDetector Registry检测器
type RegistryDetector struct{}

// detectRegistryDomain 检测Registry域名并返回域名和剩余路径
func (rd *RegistryDetector) detectRegistryDomain(c *gin.Context, path string) (string, string) {
	cfg := config.GetConfig()

	// 兼容Containerd的ns参数
	if ns := c.Query("ns"); ns != "" {
		if mapping, exists := cfg.Registries[ns]; exists && mapping.Enabled {
			return ns, path
		}
	}

	for domain := range cfg.Registries {
		if strings.HasPrefix(path, domain+"/") {
			remainingPath := strings.TrimPrefix(path, domain+"/")
			return domain, remainingPath
		}
	}

	return "", path
}

// isRegistryEnabled 检查Registry是否启用
func (rd *RegistryDetector) isRegistryEnabled(domain string) bool {
	cfg := config.GetConfig()
	if mapping, exists := cfg.Registries[domain]; exists {
		return mapping.Enabled
	}
	return false
}

// getRegistryMapping 获取Registry映射配置
func (rd *RegistryDetector) getRegistryMapping(domain string) (config.RegistryMapping, bool) {
	cfg := config.GetConfig()
	mapping, exists := cfg.Registries[domain]
	return mapping, exists && mapping.Enabled
}

var registryDetector = &RegistryDetector{}

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
		c.JSON(http.StatusOK, gin.H{})
		return
	}

	if strings.HasPrefix(path, "/v2/") {
		handleRegistryRequest(c, path)
	} else {
		c.String(http.StatusNotFound, "Docker Registry API v2 only")
	}
}

// handleRegistryRequest 处理Registry请求
func handleRegistryRequest(c *gin.Context, path string) {
	pathWithoutV2 := strings.TrimPrefix(path, "/v2/")

	if registryDomain, remainingPath := registryDetector.detectRegistryDomain(c, pathWithoutV2); registryDomain != "" {
		if registryDetector.isRegistryEnabled(registryDomain) {
			c.Set("target_registry_domain", registryDomain)
			c.Set("target_path", remainingPath)

			handleMultiRegistryRequest(c, registryDomain, remainingPath)
			return
		}
	}

	imageName, apiType, reference := parseRegistryPath(pathWithoutV2)
	if imageName == "" || apiType == "" {
		c.String(http.StatusBadRequest, "Invalid path format")
		return
	}

	if !strings.Contains(imageName, "/") {
		imageName = "library/" + imageName
	}

	if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(imageName); !allowed {
		fmt.Printf("Docker镜像 %s 访问被拒绝: %s\n", imageName, reason)
		c.String(http.StatusForbidden, "镜像访问被限制")
		return
	}

	imageRef := fmt.Sprintf("%s/%s", dockerProxy.registry.Name(), imageName)

	switch apiType {
	case "manifests":
		handleManifestRequest(c, imageRef, reference)
	case "blobs":
		handleBlobRequest(c, imageRef, reference)
	case "tags":
		handleTagsRequest(c, imageRef)
	default:
		c.String(http.StatusNotFound, "API endpoint not found")
	}
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
		c.String(http.StatusBadRequest, "Invalid reference")
		return
	}

	if c.Request.Method == http.MethodHead {
		desc, err := remote.Head(ref, dockerProxy.options...)
		if err != nil {
			fmt.Printf("HEAD请求失败: %v\n", err)
			c.String(http.StatusNotFound, "Manifest not found")
			return
		}

		c.Header("Content-Type", string(desc.MediaType))
		c.Header("Docker-Content-Digest", desc.Digest.String())
		c.Header("Content-Length", fmt.Sprintf("%d", desc.Size))
		c.Status(http.StatusOK)
	} else {
		desc, err := remote.Get(ref, dockerProxy.options...)
		if err != nil {
			fmt.Printf("GET请求失败: %v\n", err)
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
		}

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
		c.String(http.StatusBadRequest, "Invalid digest reference")
		return
	}

	layer, err := remote.Layer(digestRef, dockerProxy.options...)
	if err != nil {
		fmt.Printf("获取layer失败: %v\n", err)
		c.String(http.StatusNotFound, "Layer not found")
		return
	}

	size, err := layer.Size()
	if err != nil {
		fmt.Printf("获取layer大小失败: %v\n", err)
		c.String(http.StatusInternalServerError, "Failed to get layer size")
		return
	}

	reader, err := layer.Compressed()
	if err != nil {
		fmt.Printf("获取layer内容失败: %v\n", err)
		c.String(http.StatusInternalServerError, "Failed to get layer content")
		return
	}
	defer reader.Close()

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", fmt.Sprintf("%d", size))
	c.Header("Docker-Content-Digest", digest)

	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		fmt.Printf("复制layer内容失败: %v\n", err)
	}
}

// handleTagsRequest 处理tags列表请求
func handleTagsRequest(c *gin.Context, imageRef string) {
	repo, err := name.NewRepository(imageRef)
	if err != nil {
		fmt.Printf("解析repository失败: %v\n", err)
		c.String(http.StatusBadRequest, "Invalid repository")
		return
	}

	tags, err := remote.List(repo, dockerProxy.options...)
	if err != nil {
		fmt.Printf("获取tags失败: %v\n", err)
		c.String(http.StatusNotFound, "Tags not found")
		return
	}

	response := map[string]interface{}{
		"name": strings.TrimPrefix(imageRef, dockerProxy.registry.Name()+"/"),
		"tags": tags,
	}

	c.JSON(http.StatusOK, response)
}

// ProxyDockerAuthGin Docker认证代理
func ProxyDockerAuthGin(c *gin.Context) {
	if utils.IsTokenCacheEnabled() {
		proxyDockerAuthWithCache(c)
	} else {
		proxyDockerAuthOriginal(c)
	}
}

// proxyDockerAuthWithCache 带缓存的认证代理
func proxyDockerAuthWithCache(c *gin.Context) {
	if strings.TrimSpace(c.GetHeader("Authorization")) != "" {
		proxyDockerAuthOriginal(c)
		return
	}

	cacheKey := utils.BuildTokenCacheKey(c.Request.URL.RawQuery)

	if cachedToken := utils.GlobalCache.GetToken(cacheKey); cachedToken != "" {
		utils.WriteTokenResponse(c, cachedToken)
		return
	}

	recorder := &ResponseRecorder{
		ResponseWriter: c.Writer,
		statusCode:     200,
	}
	c.Writer = recorder

	proxyDockerAuthOriginal(c)

	if recorder.statusCode == 200 && len(recorder.body) > 0 {
		ttl := utils.ExtractTTLFromResponse(recorder.body)
		utils.GlobalCache.SetToken(cacheKey, string(recorder.body), ttl)
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
		c.String(http.StatusInternalServerError, "Failed to create request")
		return
	}

	copyProxyRequestHeaders(req.Header, c.Request.Header)

	resp, err := client.Do(req)
	if err != nil {
		c.String(http.StatusBadGateway, "Auth request failed")
		return
	}
	defer resp.Body.Close()

	proxyBaseURL := getProxyBaseURL(c)

	connectionHeaders := connectionHeaderNames(resp.Header)
	for key, values := range resp.Header {
		for _, value := range values {
			if shouldSkipProxyHeader(key, connectionHeaders) {
				continue
			}
			if strings.EqualFold(key, "WWW-Authenticate") {
				value = rewriteAuthHeader(value, proxyBaseURL)
			}
			c.Writer.Header().Add(key, value)
		}
	}

	c.Status(resp.StatusCode)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		fmt.Printf("复制认证响应失败: %v\n", err)
	}
}

func buildDockerAuthURL(c *gin.Context) (string, error) {
	if mapping, found := detectAuthRegistryMapping(c); found {
		return buildMappedAuthURL(c, mapping), nil
	}

	service := strings.TrimSpace(c.Query("service"))
	if strings.TrimSpace(c.GetHeader("Authorization")) != "" && !isDockerHubAuthService(service) {
		return "", fmt.Errorf("unsupported auth service %q", service)
	}

	authURL := "https://auth.docker.io" + c.Request.URL.Path
	if c.Request.URL.RawQuery != "" {
		authURL += "?" + c.Request.URL.RawQuery
	}
	return authURL, nil
}

func detectAuthRegistryMapping(c *gin.Context) (config.RegistryMapping, bool) {
	if targetDomain, exists := c.Get("target_registry_domain"); exists {
		if mapping, found := registryDetector.getRegistryMapping(targetDomain.(string)); found {
			return mapping, true
		}
	}

	cfg := config.GetConfig()
	for _, candidate := range []string{c.Query("service"), c.Query("ns")} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if mapping, exists := cfg.Registries[candidate]; exists && mapping.Enabled {
			return mapping, true
		}
		for _, mapping := range cfg.Registries {
			if mapping.Enabled && normalizeRegistryHost(mapping.Upstream) == candidate {
				return mapping, true
			}
		}
	}

	return config.RegistryMapping{}, false
}

func buildMappedAuthURL(c *gin.Context, mapping config.RegistryMapping) string {
	rawAuthHost := strings.TrimSpace(mapping.AuthHost)
	if rawAuthHost == "" {
		rawAuthHost = strings.TrimSpace(mapping.Upstream)
	}
	if !strings.Contains(rawAuthHost, "://") {
		rawAuthHost = "https://" + rawAuthHost
	}

	parsed, err := url.Parse(rawAuthHost)
	if err != nil || parsed.Host == "" {
		return "https://auth.docker.io" + c.Request.URL.RequestURI()
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = c.Request.URL.Path
	}
	parsed.RawQuery = c.Request.URL.RawQuery

	return parsed.String()
}

func isDockerHubAuthService(service string) bool {
	switch service {
	case "registry.docker.io", "auth.docker.io", "index.docker.io", "docker.io":
		return true
	default:
		return false
	}
}

// rewriteAuthHeader 重写认证头
func rewriteAuthHeader(authHeader, proxyBaseURL string) string {
	bearerStart := findBearerChallengeStart(authHeader)
	if bearerStart == -1 {
		return authHeader
	}

	return authHeader[:bearerStart] + rewriteAuthRealm(authHeader[bearerStart:], proxyBaseURL)
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

func rewriteAuthRealm(authHeader, proxyBaseURL string) string {
	realm := strings.TrimRight(proxyBaseURL, "/") + "/token"
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
	mapping, exists := registryDetector.getRegistryMapping(registryDomain)
	if !exists {
		c.String(http.StatusBadRequest, "Registry not configured")
		return
	}

	imageName, apiType, _ := parseRegistryPath(remainingPath)
	if imageName == "" || apiType == "" {
		c.String(http.StatusBadRequest, "Invalid path format")
		return
	}

	fullImageName := registryDomain + "/" + imageName
	if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(fullImageName); !allowed {
		fmt.Printf("镜像 %s 访问被拒绝: %s\n", fullImageName, reason)
		c.String(http.StatusForbidden, "镜像访问被限制")
		return
	}

	switch apiType {
	case "manifests":
	case "blobs":
	case "tags":
	default:
		c.String(http.StatusNotFound, "API endpoint not found")
		return
	}

	if err := proxyUpstreamRegistryRequest(c, mapping, remainingPath); err != nil {
		fmt.Printf("代理上游Registry失败: %v\n", err)
		c.String(http.StatusBadGateway, "Upstream registry request failed")
		return
	}
}

func proxyUpstreamRegistryRequest(c *gin.Context, mapping config.RegistryMapping, remainingPath string) error {
	upstreamURL, err := buildUpstreamRegistryURL(c, mapping, remainingPath)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstreamURL, c.Request.Body)
	if err != nil {
		return err
	}
	copyProxyRequestHeaders(req.Header, c.Request.Header)

	resp, err := utils.GetGlobalHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	proxyBaseURL := getProxyBaseURL(c)
	connectionHeaders := connectionHeaderNames(resp.Header)
	for key, values := range resp.Header {
		if shouldSkipProxyHeader(key, connectionHeaders) {
			continue
		}
		for _, value := range values {
			if strings.EqualFold(key, "WWW-Authenticate") {
				value = rewriteAuthHeader(value, proxyBaseURL)
			}
			c.Writer.Header().Add(key, value)
		}
	}

	c.Status(resp.StatusCode)
	if c.Request.Method == http.MethodHead {
		return nil
	}

	if _, err = io.Copy(c.Writer, resp.Body); err != nil {
		fmt.Printf("复制上游Registry响应失败: %v\n", err)
	}
	return nil
}

func buildUpstreamRegistryURL(c *gin.Context, mapping config.RegistryMapping, remainingPath string) (string, error) {
	rawUpstream := strings.TrimSpace(mapping.Upstream)
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
		return "", fmt.Errorf("invalid upstream registry %q", mapping.Upstream)
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

// handleUpstreamManifestRequest 处理上游Registry的manifest请求
func handleUpstreamManifestRequest(c *gin.Context, imageRef, reference string, mapping config.RegistryMapping) {
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
		c.String(http.StatusBadRequest, "Invalid reference")
		return
	}

	options := createUpstreamOptions(mapping)

	if c.Request.Method == http.MethodHead {
		desc, err := remote.Head(ref, options...)
		if err != nil {
			fmt.Printf("HEAD请求失败: %v\n", err)
			c.String(http.StatusNotFound, "Manifest not found")
			return
		}

		c.Header("Content-Type", string(desc.MediaType))
		c.Header("Docker-Content-Digest", desc.Digest.String())
		c.Header("Content-Length", fmt.Sprintf("%d", desc.Size))
		c.Status(http.StatusOK)
	} else {
		desc, err := remote.Get(ref, options...)
		if err != nil {
			fmt.Printf("GET请求失败: %v\n", err)
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
		}

		c.Header("Content-Type", string(desc.MediaType))
		for key, value := range headers {
			c.Header(key, value)
		}

		c.Data(http.StatusOK, string(desc.MediaType), desc.Manifest)
	}
}

// handleUpstreamBlobRequest 处理上游Registry的blob请求
func handleUpstreamBlobRequest(c *gin.Context, imageRef, digest string, mapping config.RegistryMapping) {
	digestRef, err := name.NewDigest(fmt.Sprintf("%s@%s", imageRef, digest))
	if err != nil {
		fmt.Printf("解析digest引用失败: %v\n", err)
		c.String(http.StatusBadRequest, "Invalid digest reference")
		return
	}

	options := createUpstreamOptions(mapping)
	layer, err := remote.Layer(digestRef, options...)
	if err != nil {
		fmt.Printf("获取layer失败: %v\n", err)
		c.String(http.StatusNotFound, "Layer not found")
		return
	}

	size, err := layer.Size()
	if err != nil {
		fmt.Printf("获取layer大小失败: %v\n", err)
		c.String(http.StatusInternalServerError, "Failed to get layer size")
		return
	}

	reader, err := layer.Compressed()
	if err != nil {
		fmt.Printf("获取layer内容失败: %v\n", err)
		c.String(http.StatusInternalServerError, "Failed to get layer content")
		return
	}
	defer reader.Close()

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", fmt.Sprintf("%d", size))
	c.Header("Docker-Content-Digest", digest)

	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		fmt.Printf("复制layer内容失败: %v\n", err)
	}
}

// handleUpstreamTagsRequest 处理上游Registry的tags请求
func handleUpstreamTagsRequest(c *gin.Context, imageRef string, mapping config.RegistryMapping) {
	repo, err := name.NewRepository(imageRef)
	if err != nil {
		fmt.Printf("解析repository失败: %v\n", err)
		c.String(http.StatusBadRequest, "Invalid repository")
		return
	}

	options := createUpstreamOptions(mapping)
	tags, err := remote.List(repo, options...)
	if err != nil {
		fmt.Printf("获取tags失败: %v\n", err)
		c.String(http.StatusNotFound, "Tags not found")
		return
	}

	response := map[string]interface{}{
		"name": strings.TrimPrefix(imageRef, mapping.Upstream+"/"),
		"tags": tags,
	}

	c.JSON(http.StatusOK, response)
}

// createUpstreamOptions 创建上游Registry选项
func createUpstreamOptions(mapping config.RegistryMapping) []remote.Option {
	options := []remote.Option{
		remote.WithAuth(authn.Anonymous),
		remote.WithUserAgent("hubproxy/go-containerregistry"),
		remote.WithTransport(utils.GetGlobalHTTPClient().Transport),
	}

	// 预留将来不同Registry的差异化认证逻辑扩展点
	switch mapping.AuthType {
	case "github":
	case "google":
	case "quay":
	}

	return options
}
