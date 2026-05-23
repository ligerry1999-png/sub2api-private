package middleware

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AdminRoleAccess limits scoped admin roles after AdminAuth has established identity.
// Full admins keep unrestricted access. Account managers can only operate account-pool
// maintenance endpoints and the read-only data those screens need.
func AdminRoleAccess() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, ok := GetUserRoleFromContext(c)
		if !ok {
			AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "User not found in context")
			return
		}

		if role == service.RoleAdmin {
			c.Next()
			return
		}

		if role != service.RoleAccountManager {
			AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Admin access required")
			return
		}

		if !isAccountManagerAllowedAdminRequest(c.Request.Method, c.Request.URL.Path) {
			AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Account management access required")
			return
		}

		c.Next()
	}
}

func isAccountManagerAllowedAdminRequest(method, path string) bool {
	if method == http.MethodOptions {
		return true
	}

	adminPath := adminSubpath(path)
	if adminPath == "" {
		return false
	}

	switch {
	case adminPath == "/accounts" || strings.HasPrefix(adminPath, "/accounts/"):
		// Import is allowed; export is not, because it can expose the whole account pool.
		return method != http.MethodGet || adminPath != "/accounts/data"
	case method == http.MethodGet && adminPath == "/groups/all":
		return true
	case method == http.MethodGet && adminPath == "/proxies/all":
		return true
	case strings.HasPrefix(adminPath, "/openai/"):
		return true
	case strings.HasPrefix(adminPath, "/gemini/oauth/"):
		return true
	case strings.HasPrefix(adminPath, "/antigravity/oauth/"):
		return true
	default:
		return false
	}
}

func adminSubpath(path string) string {
	idx := strings.Index(path, "/admin")
	if idx < 0 {
		return ""
	}
	sub := path[idx+len("/admin"):]
	if sub == "" {
		return "/"
	}
	return sub
}
