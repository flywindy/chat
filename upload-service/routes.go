package main

import "github.com/gin-gonic/gin"

// registerRoutes wires health plus the authenticated /api/v1 group.
func registerRoutes(r *gin.Engine, h *Handler, v TokenValidator, devMode bool) {
	r.GET("/healthz", h.HandleHealth)

	api := r.Group("/api/v1")
	api.Use(authMiddleware(v, devMode))
	api.POST("/rooms/:roomId/upload/images", h.HandleUploadImages)
	api.GET("/rooms/:roomId/image/:fileId", h.HandleDownloadImage)
}
