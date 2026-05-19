// Package swagger serves the OpenAPI spec and Swagger UI for interactive API docs.
package swagger

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed openapi.yaml index.html
var static embed.FS

// Register mounts Swagger UI at /swagger/ and the OpenAPI document at /swagger/openapi.yaml.
func Register(r gin.IRouter) {
	r.GET("/swagger/openapi.yaml", func(c *gin.Context) {
		b, err := static.ReadFile("openapi.yaml")
		if err != nil {
			c.String(http.StatusInternalServerError, "openapi spec missing")
			return
		}
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", b)
	})

	r.GET("/swagger", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/swagger/")
	})

	r.GET("/swagger/", func(c *gin.Context) {
		b, err := static.ReadFile("index.html")
		if err != nil {
			c.String(http.StatusInternalServerError, "swagger ui missing")
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", b)
	})
}
