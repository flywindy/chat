package main

import "github.com/gin-gonic/gin"

func registerRoutes(r *gin.Engine, h *PortalHandler) {
	r.GET("/api/userInfo", h.HandleUserInfo)
	r.GET("/api/settings", h.HandleSettings)
	r.GET("/healthz", h.HandleHealth)
	r.GET("/readyz", h.HandleReady)
}
