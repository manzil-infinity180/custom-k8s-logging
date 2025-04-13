package routes

import (
	"github.com/gin-gonic/gin"
	"github.com/manzil-infinity180/k8s-custom-logger/logs"
	"net/http"
)

// SetupRoutes initializes all API routes
func setupResourceRoutes(router *gin.Engine) {
	// TODO: make it to support the custom API Resource
	// TODO: make it to support the core API Resource in namespace / without namespace (wide-cluster resource)
	// TODO: add logic to check - is this is core API ? or not and based on this make request on it
	api := router.Group("/api")
	{
		api.GET("/:resourceKind/:namespace/log", logs.LogWorkloads)
		api.GET("/health", func(c *gin.Context) {
			c.JSON(http.StatusAccepted, gin.H{
				"status": "yep!!! bro i am running!!!",
			})
		})
	}
}
