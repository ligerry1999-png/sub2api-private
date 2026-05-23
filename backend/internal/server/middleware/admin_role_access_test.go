//go:build unit

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAdminRoleAccessAccountManagerScope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		role string
		path string
		want int
	}{
		{name: "full admin can access image logs", role: service.RoleAdmin, path: "/api/v1/admin/image-logs", want: http.StatusOK},
		{name: "account manager can list accounts", role: service.RoleAccountManager, path: "/api/v1/admin/accounts", want: http.StatusOK},
		{name: "account manager can import account data", role: service.RoleAccountManager, path: "/api/v1/admin/accounts/data", want: http.StatusOK},
		{name: "account manager cannot export account data", role: service.RoleAccountManager, path: "/api/v1/admin/accounts/data", want: http.StatusForbidden},
		{name: "account manager can read groups for account form", role: service.RoleAccountManager, path: "/api/v1/admin/groups/all", want: http.StatusOK},
		{name: "account manager cannot access image logs", role: service.RoleAccountManager, path: "/api/v1/admin/image-logs", want: http.StatusForbidden},
		{name: "regular user cannot access admin accounts", role: service.RoleUser, path: "/api/v1/admin/accounts", want: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := http.MethodGet
			if tt.name == "account manager can import account data" {
				method = http.MethodPost
			}

			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set(string(ContextKeyUserRole), tt.role)
			})
			router.Use(AdminRoleAccess())
			router.Handle(method, tt.path, func(c *gin.Context) {
				c.Status(http.StatusOK)
			})

			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, tt.path, nil)
			router.ServeHTTP(w, req)

			require.Equal(t, tt.want, w.Code)
		})
	}
}
