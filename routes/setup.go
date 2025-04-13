package routes

import (
	"github.com/gin-gonic/gin"
)

func SetupRoutes(router *gin.Engine) {
	// Initialize all route groups
	setupResourceRoutes(router)
}
