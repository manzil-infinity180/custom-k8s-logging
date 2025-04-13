package main

import (
	"github.com/gin-gonic/gin"
	"github.com/manzil-infinity180/k8s-custom-logger/routes"
	"log"
)

// npx wscat -c ws://localhost:4000/api/:resourceKind/:namespace/log
// example = /api/deployments/nginx/log
func main() {
	router := gin.Default()

	routes.SetupRoutes(router)

	if err := router.Run(":4000"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// TODO
// we are adding the logic to check is that namespaces is present or not
// adding the summary message before getting listing for the next update on the workloads
// we will saving the logs (previous) into some files so that we can read it from that files
// Adding more conditions check into the updateFunc
//
// not sure we can cache the logs for the websocket or not
// go bit deep into the logic how the logs are in the pods
// our whole assumption is on the things that we do not have pods in our context
