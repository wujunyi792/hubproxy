package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestNormalizeRepository(t *testing.T) {
	official := &Repository{Name: "nginx", IsOfficial: true}
	normalizeRepository(official)
	if official.Namespace != "library" || official.Name != "library/nginx" {
		t.Fatalf("official normalized to %#v", official)
	}

	userRepo := &Repository{Name: "owner/app", RepoOwner: "owner"}
	normalizeRepository(userRepo)
	if userRepo.Namespace != "owner" || userRepo.Name != "app" {
		t.Fatalf("user repo normalized to %#v", userRepo)
	}
}

func TestParsePaginationParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodGet, "/?page=3&page_size=50", nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	page, pageSize := parsePaginationParams(c, 25)
	if page != 3 || pageSize != 50 {
		t.Fatalf("pagination = %d %d", page, pageSize)
	}
}

func TestSearchCacheExpires(t *testing.T) {
	cache := &Cache{data: make(map[string]cacheEntry), maxSize: 10}
	cache.SetWithTTL("k", "v", -time.Second)

	if got, ok := cache.Get("k"); ok || got != nil {
		t.Fatalf("expired cache returned: %#v", got)
	}
}
