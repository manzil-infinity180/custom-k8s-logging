package logs

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // Windows
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func tweakListOptions(name string) dynamicinformer.TweakListOptionsFunc {
	// Filter by resource name
	if name != "" {
		return func(options *metav1.ListOptions) {
			options.FieldSelector = fmt.Sprintf("metadata.name=%s", name)
		}
	}
	return nil
}

func LogWorkloads(c *gin.Context) {
	cookieContext, err := c.Cookie("ui-wds-context")
	if err != nil {
		cookieContext = "wds1"
	}
	clientset, dynamicClient, err := GetClientSetWithContext(cookieContext)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// websocket connection
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("WebSocket Upgrade Error:", err)
		return
	}
	defer conn.Close()

	resourceKind := c.Param("resourceKind")
	namespace := c.Param("namespace")
	name := c.Query("name")

	timestamp := time.Now().Format(time.RFC3339)
	if namespace == "" {

		message := fmt.Sprintf("[%s] %s", timestamp, fmt.Sprintf("no namespace exists with name %s", namespace))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
			log.Println("Error writing to WebSocket:", err)
		}
		return
	}
	// checking for the namespace exists
	_, err = clientset.CoreV1().Namespaces().Get(c, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		message := fmt.Sprintf("[%s] %s", timestamp, fmt.Sprintf("no namespace exists with name %s", namespace))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
			log.Println("Error writing to WebSocket:", err)
		}
		return
	}
	if err != nil {
		message := fmt.Sprintf("[%s] %s", timestamp, "no namespace exists with name")
		if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
			log.Println("Error writing to WebSocket:", err)
		}
		return
	}
	discoveryClient := clientset.Discovery()
	gvr, _, err := getGVR(discoveryClient, resourceKind)
	if err != nil {
		message := fmt.Sprintf("[%s] %s", timestamp, "error while retreiving the gvr")
		if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
			log.Println("Error writing to WebSocket:", err)
		}
		return
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, time.Minute, namespace, tweakListOptions(name))
	//factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, time.Minute, namespace, func(options *metav1.ListOptions) {
	//	options.FieldSelector = fmt.Sprintf("metadata.name=%s", name) // Filter by resource name
	//})
	informer := factory.ForResource(gvr).Informer()

	mux := &sync.RWMutex{}
	synced := false
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			mux.RLock()
			defer mux.RUnlock()
			if !synced {
				return
			}

			item, ok := obj.(*unstructured.Unstructured)
			if !ok {
				log.Printf("item is not *unstructured.Unstructured")
				return
			}
			uid := string(item.GetUID())
			gvk := item.GroupVersionKind()
			timestamp := time.Now().Format(time.RFC3339)
			message := fmt.Sprintf("[%s] ADDED: Kind=%s, Name=%s, Namespace=%s, UID=%s",
				timestamp, gvk.Kind, item.GetName(), namespace, uid)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
				log.Println("Error writing to WebSocket:", err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			mux.RLock()
			defer mux.RUnlock()
			if !synced {
				return
			}
			old, ok := oldObj.(*unstructured.Unstructured)
			if !ok {
				log.Printf("item is not *unstructured.Unstructured")
				return
			}
			new, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				log.Printf("item is not *unstructured.Unstructured")
				return
			}
			uid := string(old.GetUID())
			gvk := old.GroupVersionKind()
			if old.GetResourceVersion() == new.GetResourceVersion() {
				return
			}

			// TODO: Improve the logs information and add some valuable messages
			timestamp := time.Now().Format(time.RFC3339)
			message := fmt.Sprintf("[%s] UPDATED: Kind=%s, Name=%s, Namespace=%s, UID=%s",
				timestamp, gvk.Kind, old.GetName(), old.GetNamespace(), uid)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
				log.Println("Error writing to WebSocket:", err)
			}
			// updated logs for the labels
			labelsOld := old.GetLabels()
			labelsNew := new.GetLabels()
			if !reflect.DeepEqual(labelsOld, labelsNew) {
				labelMsg := fmt.Sprintf("[%s] LABELS CHANGED: %v → %v", timestamp, labelsOld, labelsNew)
				if err := conn.WriteMessage(websocket.TextMessage, []byte(labelMsg)); err != nil {
					log.Println("Error writing label change to WebSocket:", err)
				}
			}
			// Logs for the Deployment
			if strings.EqualFold(gvk.Kind, "Deployment") {
				oldReplicas, _, err1 := unstructured.NestedInt64(old.Object, "spec", "replicas")
				newReplicas, _, err2 := unstructured.NestedInt64(new.Object, "spec", "replicas")

				if err1 == nil && err2 == nil && oldReplicas != newReplicas {
					replicaMsg := fmt.Sprintf("[%s] REPLICAS CHANGED: %d → %d", timestamp, oldReplicas, newReplicas)
					if err := conn.WriteMessage(websocket.TextMessage, []byte(replicaMsg)); err != nil {
						log.Println("Error writing replica change to WebSocket:", err)
					}
				}
				specOld, _, _ := unstructured.NestedMap(old.Object, "spec", "template", "spec")
				specNew, _, _ := unstructured.NestedMap(new.Object, "spec", "template", "spec")

				containersOld, _, _ := unstructured.NestedSlice(specOld, "containers")
				containersNew, _, _ := unstructured.NestedSlice(specNew, "containers")

				for i := range containersOld {
					oldC := containersOld[i].(map[string]interface{})
					newC := containersNew[i].(map[string]interface{})
					oldImage := oldC["image"].(string)
					newImage := newC["image"].(string)
					if oldImage != newImage {
						msg := fmt.Sprintf("[%s] CONTAINER IMAGE UPDATED in %s: %s → %s", timestamp, oldC["name"], oldImage, newImage)
						conn.WriteMessage(websocket.TextMessage, []byte(msg))
					}
				}

				for i := range containersOld {
					oldRes := containersOld[i].(map[string]interface{})["resources"]
					newRes := containersNew[i].(map[string]interface{})["resources"]
					if !reflect.DeepEqual(oldRes, newRes) {
						msg := fmt.Sprintf("[%s] RESOURCE REQUEST/LIMIT CHANGED in %s", timestamp, containersOld[i].(map[string]interface{})["name"])
						conn.WriteMessage(websocket.TextMessage, []byte(msg))
					}
				}
			}

			if strings.EqualFold(gvk.Kind, "Service") {
				oldTypes, _, err1 := unstructured.NestedString(old.Object, "spec", "type")
				newTypes, _, err2 := unstructured.NestedString(new.Object, "spec", "type")

				if err1 == nil && err2 == nil && oldTypes != newTypes {
					replicaMsg := fmt.Sprintf("[%s] SERVICE TYPES CHANGED: %s → %s", timestamp, oldTypes, newTypes)
					if err := conn.WriteMessage(websocket.TextMessage, []byte(replicaMsg)); err != nil {
						log.Println("Error writing service type change to WebSocket:", err)
					}
				}
			}

		},
		DeleteFunc: func(obj interface{}) {
			mux.RLock()
			defer mux.RUnlock()
			if !synced {
				return
			}
			item, ok := obj.(*unstructured.Unstructured)
			if !ok {
				log.Printf("item is not *unstructured.Unstructured")
				return
			}
			uid := string(item.GetUID())
			gvk := item.GroupVersionKind()
			timestamp := time.Now().Format(time.RFC3339)
			message := fmt.Sprintf("[%s] DELETED: Kind=%s, Name=%s, Namespace=%s, UID=%s",
				timestamp, gvk.Kind, item.GetName(), namespace, uid)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
				log.Println("Error writing to WebSocket:", err)
			}
		},
	})

	// TODO: Optimize the websocket connection and handle the interrupt properly
	//ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	//defer cancel()

	go informer.Run(c.Done())

	isSynced := cache.WaitForCacheSync(c.Done(), informer.HasSynced)
	mux.Lock()
	synced = isSynced
	mux.Unlock()
	if !isSynced {
		log.Fatal("failed to sync")
	}
	<-c.Done()
}

func LogWorkloads2(c *gin.Context) {
	cookieContext, err := c.Cookie("ui-wds-context")
	if err != nil {
		cookieContext = "wds1"
	}
	clientset, dynamicClient, err := GetClientSetWithContext(cookieContext)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// websocket connection
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("WebSocket Upgrade Error:", err)
		return
	}
	defer conn.Close()

	resourceKind := c.Param("resourceKind")
	namespace := c.Param("namespace")
	name := c.Query("name")

	if namespace == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("no namespace exists with name %s", namespace)})
		return
	}

	discoveryClient := clientset.Discovery()
	gvr, _, err := getGVR(discoveryClient, resourceKind)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported resource type"})
		return
	}

	// Create informer factory filtering by name if provided
	tweakListOptions := func(options *metav1.ListOptions) {
		if name != "" {
			options.FieldSelector = fmt.Sprintf("metadata.name=%s", name)
		}
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, time.Minute, namespace, tweakListOptions)
	informer := factory.ForResource(gvr).Informer()

	mux := &sync.RWMutex{}
	synced := false

	// Helper functions for sending messages
	sendMessage := func(msgType string, format string, args ...interface{}) {
		timestamp := time.Now().Format(time.RFC3339)
		prefix := fmt.Sprintf("[%s] %s: ", timestamp, msgType)
		message := prefix + fmt.Sprintf(format, args...)

		if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
			log.Printf("Error writing to WebSocket: %v", err)
		}
	}

	// Helper for getting nested values with proper error handling
	getNestedValue := func(obj map[string]interface{}, valueType string, keys ...string) (interface{}, bool) {
		var value interface{}
		var exists bool
		var err error

		switch valueType {
		case "string":
			value, exists, err = unstructured.NestedString(obj, keys...)
		case "int64":
			value, exists, err = unstructured.NestedInt64(obj, keys...)
		case "map":
			value, exists, err = unstructured.NestedMap(obj, keys...)
		case "slice":
			value, exists, err = unstructured.NestedSlice(obj, keys...)
		case "bool":
			value, exists, err = unstructured.NestedBool(obj, keys...)
		}

		if err != nil {
			log.Printf("Error getting nested value for %v: %v", keys, err)
			return nil, false
		}
		return value, exists
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			mux.RLock()
			defer mux.RUnlock()
			if !synced {
				return
			}

			item, ok := obj.(*unstructured.Unstructured)
			if !ok {
				log.Printf("item is not *unstructured.Unstructured")
				return
			}

			gvk := item.GroupVersionKind()
			objectName := item.GetName()
			objectNamespace := item.GetNamespace()
			uid := string(item.GetUID())

			sendMessage("ADDED", "Kind=%s, Name=%s, Namespace=%s, UID=%s",
				gvk.Kind, objectName, objectNamespace, uid)

			// Resource-specific additional information on creation
			switch {
			case strings.EqualFold(gvk.Kind, "Deployment"):
				if replicas, exists, _ := unstructured.NestedInt64(item.Object, "spec", "replicas"); exists {
					sendMessage("INFO", "Deployment %s created with %d replicas", objectName, replicas)
				}

				// Log container images on creation
				if containers, ok := getNestedValue(item.Object, "slice", "spec", "template", "spec", "containers"); ok && containers != nil {
					for i, c := range containers.([]interface{}) {
						container := c.(map[string]interface{})
						containerName := container["name"].(string)
						image := container["image"].(string)
						sendMessage("INFO", "Container #%d: %s using image %s", i+1, containerName, image)
					}
				}

			case strings.EqualFold(gvk.Kind, "Service"):
				if serviceType, exists, _ := unstructured.NestedString(item.Object, "spec", "type"); exists {
					sendMessage("INFO", "Service %s created with type %s", objectName, serviceType)
				}

				// Log service ports
				if ports, ok := getNestedValue(item.Object, "slice", "spec", "ports"); ok && ports != nil {
					for _, p := range ports.([]interface{}) {
						port := p.(map[string]interface{})
						portStr := fmt.Sprintf("%v", port["port"])
						protocol := "TCP"
						if proto, exists := port["protocol"]; exists {
							protocol = proto.(string)
						}
						sendMessage("INFO", "Service port: %s/%s", portStr, protocol)
					}
				}

			case strings.EqualFold(gvk.Kind, "ConfigMap"):
				if data, ok := getNestedValue(item.Object, "map", "data"); ok && data != nil {
					keys := make([]string, 0, len(data.(map[string]interface{})))
					for k := range data.(map[string]interface{}) {
						keys = append(keys, k)
					}
					sendMessage("INFO", "ConfigMap %s created with keys: %s", objectName, strings.Join(keys, ", "))
				}

			case strings.EqualFold(gvk.Kind, "Secret"):
				if data, ok := getNestedValue(item.Object, "map", "data"); ok && data != nil {
					keys := make([]string, 0, len(data.(map[string]interface{})))
					for k := range data.(map[string]interface{}) {
						keys = append(keys, k)
					}
					sendMessage("INFO", "Secret %s created with keys: %s", objectName, strings.Join(keys, ", "))
				}
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			mux.RLock()
			defer mux.RUnlock()
			if !synced {
				return
			}

			old, ok := oldObj.(*unstructured.Unstructured)
			if !ok {
				log.Printf("item is not *unstructured.Unstructured")
				return
			}
			new, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				log.Printf("item is not *unstructured.Unstructured")
				return
			}

			// Skip if resource version hasn't changed
			if old.GetResourceVersion() == new.GetResourceVersion() {
				return
			}

			uid := string(old.GetUID())
			gvk := old.GroupVersionKind()
			objectName := old.GetName()
			objectNamespace := old.GetNamespace()

			sendMessage("UPDATED", "Kind=%s, Name=%s, Namespace=%s, UID=%s",
				gvk.Kind, objectName, objectNamespace, uid)

			// Check for label changes
			labelsOld := old.GetLabels()
			labelsNew := new.GetLabels()
			if !reflect.DeepEqual(labelsOld, labelsNew) {
				// Find added, removed, and changed labels
				added := []string{}
				changed := []string{}
				removed := []string{}

				for k, v := range labelsNew {
					oldVal, exists := labelsOld[k]
					if !exists {
						added = append(added, fmt.Sprintf("%s=%s", k, v))
					} else if oldVal != v {
						changed = append(changed, fmt.Sprintf("%s: %s → %s", k, oldVal, v))
					}
				}

				for k := range labelsOld {
					if _, exists := labelsNew[k]; !exists {
						removed = append(removed, k)
					}
				}

				if len(added) > 0 {
					sendMessage("LABELS ADDED", "%s", strings.Join(added, ", "))
				}
				if len(changed) > 0 {
					sendMessage("LABELS CHANGED", "%s", strings.Join(changed, ", "))
				}
				if len(removed) > 0 {
					sendMessage("LABELS REMOVED", "%s", strings.Join(removed, ", "))
				}
			}

			// Resource-specific update checks
			switch {
			case strings.EqualFold(gvk.Kind, "Deployment"):
				// Check for replicas changes
				oldReplicas, oldExists, _ := unstructured.NestedInt64(old.Object, "spec", "replicas")
				newReplicas, newExists, _ := unstructured.NestedInt64(new.Object, "spec", "replicas")

				if oldExists && newExists && oldReplicas != newReplicas {
					sendMessage("REPLICAS CHANGED", "%s: %d → %d", objectName, oldReplicas, newReplicas)
				}

				// Check for container image changes
				oldSpec, oldSpecExists, _ := unstructured.NestedMap(old.Object, "spec", "template", "spec")
				newSpec, newSpecExists, _ := unstructured.NestedMap(new.Object, "spec", "template", "spec")

				if oldSpecExists && newSpecExists {
					oldContainers, oldContExists, _ := unstructured.NestedSlice(oldSpec, "containers")
					newContainers, newContExists, _ := unstructured.NestedSlice(newSpec, "containers")

					if oldContExists && newContExists {
						// Build a map for old containers by name for easy lookup
						oldContainerMap := make(map[string]map[string]interface{})
						for _, c := range oldContainers {
							container := c.(map[string]interface{})
							name := container["name"].(string)
							oldContainerMap[name] = container
						}

						// Check each new container against old ones
						for _, c := range newContainers {
							container := c.(map[string]interface{})
							name := container["name"].(string)
							newImage := container["image"].(string)

							if oldContainer, exists := oldContainerMap[name]; exists {
								oldImage := oldContainer["image"].(string)
								if oldImage != newImage {
									sendMessage("CONTAINER IMAGE", "%s: %s → %s", name, oldImage, newImage)
								}

								// Check resource requests/limits
								oldResources, oldResExists := oldContainer["resources"]
								newResources, newResExists := container["resources"]

								if oldResExists && newResExists && !reflect.DeepEqual(oldResources, newResources) {
									oldResMap := oldResources.(map[string]interface{})
									newResMap := newResources.(map[string]interface{})

									// Check requests
									if oldReq, oldExists := oldResMap["requests"]; oldExists {
										if newReq, newExists := newResMap["requests"]; newExists {
											if !reflect.DeepEqual(oldReq, newReq) {
												sendMessage("RESOURCE REQUESTS", "%s resources updated", name)

												// Detailed CPU/memory changes
												oldReqMap := oldReq.(map[string]interface{})
												newReqMap := newReq.(map[string]interface{})

												if oldCPU, exists := oldReqMap["cpu"]; exists {
													if newCPU, exists := newReqMap["cpu"]; exists && oldCPU != newCPU {
														sendMessage("CPU REQUEST", "%s: %v → %v", name, oldCPU, newCPU)
													}
												}

												if oldMem, exists := oldReqMap["memory"]; exists {
													if newMem, exists := newReqMap["memory"]; exists && oldMem != newMem {
														sendMessage("MEMORY REQUEST", "%s: %v → %v", name, oldMem, newMem)
													}
												}
											}
										}
									}

									// Check limits
									if oldLim, oldExists := oldResMap["limits"]; oldExists {
										if newLim, newExists := newResMap["limits"]; newExists {
											if !reflect.DeepEqual(oldLim, newLim) {
												sendMessage("RESOURCE LIMITS", "%s limits updated", name)

												// Detailed CPU/memory changes
												oldLimMap := oldLim.(map[string]interface{})
												newLimMap := newLim.(map[string]interface{})

												if oldCPU, exists := oldLimMap["cpu"]; exists {
													if newCPU, exists := newLimMap["cpu"]; exists && oldCPU != newCPU {
														sendMessage("CPU LIMIT", "%s: %v → %v", name, oldCPU, newCPU)
													}
												}

												if oldMem, exists := oldLimMap["memory"]; exists {
													if newMem, exists := newLimMap["memory"]; exists && oldMem != newMem {
														sendMessage("MEMORY LIMIT", "%s: %v → %v", name, oldMem, newMem)
													}
												}
											}
										}
									}
								}
							} else {
								// New container added
								sendMessage("CONTAINER ADDED", "%s with image %s", name, newImage)
							}
						}

						// Check for removed containers
						for _, c := range oldContainers {
							container := c.(map[string]interface{})
							name := container["name"].(string)

							found := false
							for _, nc := range newContainers {
								newContainer := nc.(map[string]interface{})
								if newContainer["name"] == name {
									found = true
									break
								}
							}

							if !found {
								sendMessage("CONTAINER REMOVED", "%s", name)
							}
						}
					}
				}

				// Check for status changes
				oldStatus, oldStatusExists, _ := unstructured.NestedMap(old.Object, "status")
				newStatus, newStatusExists, _ := unstructured.NestedMap(new.Object, "status")

				if oldStatusExists && newStatusExists {
					oldAvail, oldAvailExists, _ := unstructured.NestedInt64(oldStatus, "availableReplicas")
					newAvail, newAvailExists, _ := unstructured.NestedInt64(newStatus, "availableReplicas")

					if oldAvailExists && newAvailExists && oldAvail != newAvail {
						sendMessage("AVAILABILITY", "%s: Available replicas %d → %d", objectName, oldAvail, newAvail)
					}

					// Check conditions
					oldCond, oldCondExists, _ := unstructured.NestedSlice(oldStatus, "conditions")
					newCond, newCondExists, _ := unstructured.NestedSlice(newStatus, "conditions")

					if oldCondExists && newCondExists {
						oldCondMap := make(map[string]map[string]interface{})
						for _, c := range oldCond {
							condition := c.(map[string]interface{})
							condType := condition["type"].(string)
							oldCondMap[condType] = condition
						}

						for _, c := range newCond {
							condition := c.(map[string]interface{})
							condType := condition["type"].(string)
							status := condition["status"].(string)

							if oldCondition, exists := oldCondMap[condType]; exists {
								oldStatus := oldCondition["status"].(string)
								if oldStatus != status {
									sendMessage("CONDITION", "%s: %s changed from %s to %s", objectName, condType, oldStatus, status)
									if reason, exists := condition["reason"]; exists {
										sendMessage("REASON", "%s: %s", condType, reason)
									}
								}
							}
						}
					}
				}

			case strings.EqualFold(gvk.Kind, "Service"):
				// Check for service type changes
				oldType, oldTypeExists, _ := unstructured.NestedString(old.Object, "spec", "type")
				newType, newTypeExists, _ := unstructured.NestedString(new.Object, "spec", "type")

				if oldTypeExists && newTypeExists && oldType != newType {
					sendMessage("SERVICE TYPE", "%s: %s → %s", objectName, oldType, newType)
				}

				// Check for port changes
				oldPorts, oldPortsExists, _ := unstructured.NestedSlice(old.Object, "spec", "ports")
				newPorts, newPortsExists, _ := unstructured.NestedSlice(new.Object, "spec", "ports")

				if oldPortsExists && newPortsExists && !reflect.DeepEqual(oldPorts, newPorts) {
					sendMessage("PORTS CHANGED", "%s service ports updated", objectName)

					// Map old ports by port number for comparison
					oldPortMap := make(map[int64]map[string]interface{})
					for _, p := range oldPorts {
						port := p.(map[string]interface{})
						if portNum, ok := port["port"].(int64); ok {
							oldPortMap[portNum] = port
						}
					}

					// Check new ports
					for _, p := range newPorts {
						port := p.(map[string]interface{})
						portNum, _ := port["port"].(int64)

						if oldPort, exists := oldPortMap[portNum]; exists {
							// Compare existing port details
							if !reflect.DeepEqual(oldPort, port) {
								sendMessage("PORT UPDATE", "Port %d configuration changed", portNum)
							}
						} else {
							// New port added
							protocol := "TCP"
							if proto, exists := port["protocol"]; exists {
								protocol = proto.(string)
							}
							sendMessage("PORT ADDED", "%d/%s", portNum, protocol)
						}
					}

					// Check for removed ports
					for portNum := range oldPortMap {
						found := false
						for _, p := range newPorts {
							port := p.(map[string]interface{})
							if newPortNum, ok := port["port"].(int64); ok && newPortNum == portNum {
								found = true
								break
							}
						}

						if !found {
							sendMessage("PORT REMOVED", "%d", portNum)
						}
					}
				}

			case strings.EqualFold(gvk.Kind, "ConfigMap"):
				// Check for data changes
				oldData, oldDataExists, _ := unstructured.NestedMap(old.Object, "data")
				newData, newDataExists, _ := unstructured.NestedMap(new.Object, "data")

				if oldDataExists && newDataExists {
					// Find added, changed, and removed keys
					added := []string{}
					changed := []string{}
					removed := []string{}

					for k := range newData {
						if _, exists := oldData[k]; !exists {
							added = append(added, k)
						}
					}

					for k := range oldData {
						if _, exists := newData[k]; !exists {
							removed = append(removed, k)
						} else if !reflect.DeepEqual(oldData[k], newData[k]) {
							changed = append(changed, k)
						}
					}

					if len(added) > 0 {
						sendMessage("CONFIG ADDED", "%s: Added keys: %s", objectName, strings.Join(added, ", "))
					}
					if len(changed) > 0 {
						sendMessage("CONFIG MODIFIED", "%s: Modified keys: %s", objectName, strings.Join(changed, ", "))
					}
					if len(removed) > 0 {
						sendMessage("CONFIG REMOVED", "%s: Removed keys: %s", objectName, strings.Join(removed, ", "))
					}
				}

			case strings.EqualFold(gvk.Kind, "Secret"):
				// Only report changes to keys, not values (for security)
				oldData, oldDataExists, _ := unstructured.NestedMap(old.Object, "data")
				newData, newDataExists, _ := unstructured.NestedMap(new.Object, "data")

				if oldDataExists && newDataExists {
					oldKeys := make([]string, 0, len(oldData))
					for k := range oldData {
						oldKeys = append(oldKeys, k)
					}

					newKeys := make([]string, 0, len(newData))
					for k := range newData {
						newKeys = append(newKeys, k)
					}

					sort.Strings(oldKeys)
					sort.Strings(newKeys)

					if !reflect.DeepEqual(oldKeys, newKeys) {
						sendMessage("SECRET KEYS", "%s: Secret keys have been modified", objectName)
					} else {
						// Keys are the same, but value(s) changed
						for k := range oldData {
							if !reflect.DeepEqual(oldData[k], newData[k]) {
								sendMessage("SECRET VALUE", "%s: Value for key '%s' has been changed", objectName, k)
								break
							}
						}
					}
				}

			case strings.EqualFold(gvk.Kind, "StatefulSet"), strings.EqualFold(gvk.Kind, "DaemonSet"):
				// Similar to Deployment updates
				oldReplicas, oldExists, _ := unstructured.NestedInt64(old.Object, "spec", "replicas")
				newReplicas, newExists, _ := unstructured.NestedInt64(new.Object, "spec", "replicas")

				if oldExists && newExists && oldReplicas != newReplicas {
					sendMessage("REPLICAS CHANGED", "%s %s: %d → %d", gvk.Kind, objectName, oldReplicas, newReplicas)
				}

				// Status updates
				oldStatus, oldStatusExists, _ := unstructured.NestedMap(old.Object, "status")
				newStatus, newStatusExists, _ := unstructured.NestedMap(new.Object, "status")

				if oldStatusExists && newStatusExists {
					oldReady, oldReadyExists, _ := unstructured.NestedInt64(oldStatus, "readyReplicas")
					newReady, newReadyExists, _ := unstructured.NestedInt64(newStatus, "readyReplicas")

					if oldReadyExists && newReadyExists && oldReady != newReady {
						sendMessage("READY REPLICAS", "%s %s: %d → %d", gvk.Kind, objectName, oldReady, newReady)
					}
				}

			case strings.EqualFold(gvk.Kind, "Ingress"):
				// Check for host changes
				oldRules, oldRulesExists, _ := unstructured.NestedSlice(old.Object, "spec", "rules")
				newRules, newRulesExists, _ := unstructured.NestedSlice(new.Object, "spec", "rules")

				if oldRulesExists && newRulesExists && !reflect.DeepEqual(oldRules, newRules) {
					oldHosts := []string{}
					for _, r := range oldRules {
						rule := r.(map[string]interface{})
						if host, exists := rule["host"]; exists {
							oldHosts = append(oldHosts, host.(string))
						}
					}

					newHosts := []string{}
					for _, r := range newRules {
						rule := r.(map[string]interface{})
						if host, exists := rule["host"]; exists {
							newHosts = append(newHosts, host.(string))
						}
					}

					sort.Strings(oldHosts)
					sort.Strings(newHosts)

					if !reflect.DeepEqual(oldHosts, newHosts) {
						sendMessage("INGRESS HOSTS", "%s: Host configuration changed", objectName)
						sendMessage("HOSTS CHANGED", "Before: %s, After: %s", strings.Join(oldHosts, ", "), strings.Join(newHosts, ", "))
					} else {
						sendMessage("INGRESS RULES", "%s: Path rules have been updated", objectName)
					}
				}

				// Check for TLS changes
				oldTLS, oldTLSExists, _ := unstructured.NestedSlice(old.Object, "spec", "tls")
				newTLS, newTLSExists, _ := unstructured.NestedSlice(new.Object, "spec", "tls")

				if (!oldTLSExists && newTLSExists) || (oldTLSExists && !newTLSExists) {
					sendMessage("TLS CONFIG", "%s: TLS configuration %s", objectName, "added")
					sendMessage("TLS CONFIG", "%s: TLS configuration %s", objectName, "removed")
				} else if oldTLSExists && newTLSExists && !reflect.DeepEqual(oldTLS, newTLS) {
					sendMessage("TLS CONFIG", "%s: TLS configuration modified", objectName)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			mux.RLock()
			defer mux.RUnlock()
			if !synced {
				return
			}

			item, ok := obj.(*unstructured.Unstructured)
			if !ok {
				log.Printf("item is not *unstructured.Unstructured")
				return
			}

			gvk := item.GroupVersionKind()
			objectName := item.GetName()
			objectNamespace := item.GetNamespace()
			uid := string(item.GetUID())

			sendMessage("DELETED", "Kind=%s, Name=%s, Namespace=%s, UID=%s",
				gvk.Kind, objectName, objectNamespace, uid)

			// Resource-specific deletion messages
			switch {
			case strings.EqualFold(gvk.Kind, "Service"):
				sendMessage("SERVICE REMOVED", "Service %s in namespace %s was deleted", objectName, objectNamespace)

			case strings.EqualFold(gvk.Kind, "Deployment"):
				sendMessage("DEPLOYMENT REMOVED", "Deployment %s in namespace %s was deleted", objectName, objectNamespace)

			case strings.EqualFold(gvk.Kind, "ConfigMap"):
				sendMessage("CONFIG REMOVED", "ConfigMap %s in namespace %s was deleted", objectName, objectNamespace)

			case strings.EqualFold(gvk.Kind, "Secret"):
				sendMessage("SECRET REMOVED", "Secret %s in namespace %s was deleted", objectName, objectNamespace)

			case strings.EqualFold(gvk.Kind, "Ingress"):
				sendMessage("INGRESS REMOVED", "Ingress %s in namespace %s was deleted", objectName, objectNamespace)
			}
		},
	})

	go informer.Run(c.Done())

	isSynced := cache.WaitForCacheSync(c.Done(), informer.HasSynced)
	mux.Lock()
	synced = isSynced
	mux.Unlock()

	if !isSynced {
		sendMessage("ERROR", "Failed to sync informer cache")
		log.Println("Failed to sync informer cache")
		return
	}

	sendMessage("INFO", "Started monitoring %s in namespace %s", resourceKind, namespace)
	if name != "" {
		sendMessage("INFO", "Filtered to resource name: %s", name)
	}

	<-c.Done()
	sendMessage("INFO", "Stopping monitoring session")
}

// GetClientSetWithContext retrieves a Kubernetes clientset and dynamic client for a specified context
func GetClientSetWithContext(contextName string) (*kubernetes.Clientset, dynamic.Interface, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		if home := homeDir(); home != "" {
			kubeconfig = fmt.Sprintf("%s/.kube/config", home)
		}
	}

	// Load the kubeconfig file
	config, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load kubeconfig: %v", err)
	}

	// Check if the specified context exists
	ctxContext := config.Contexts[contextName]
	if ctxContext == nil {
		return nil, nil, fmt.Errorf("failed to find context '%s'", contextName)
	}

	// Create config for the specified context
	clientConfig := clientcmd.NewDefaultClientConfig(
		*config,
		&clientcmd.ConfigOverrides{
			CurrentContext: contextName,
		},
	)

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create restconfig: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create Kubernetes client: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create dynamic client: %v", err)
	}

	return clientset, dynamicClient, nil
}

// mapResourceToGVR maps resource types to their GroupVersionResource (GVR)
func getGVR(discoveryClient discovery.DiscoveryInterface, resourceKind string) (schema.GroupVersionResource, bool, error) {
	resourceList, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		return schema.GroupVersionResource{}, false, err
	}

	for _, resourceGroup := range resourceList {
		for _, resource := range resourceGroup.APIResources {
			// we are looking for the resourceKind
			if strings.EqualFold(resource.Kind, resourceKind) {
				gv, err := schema.ParseGroupVersion(resourceGroup.GroupVersion)
				if err != nil {
					return schema.GroupVersionResource{}, false, err
				}
				isNamespaced := resource.Namespaced
				return schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: resource.Name}, isNamespaced, nil
			} else if strings.EqualFold(resource.Name, resourceKind) {
				gv, err := schema.ParseGroupVersion(resourceGroup.GroupVersion)
				if err != nil {
					return schema.GroupVersionResource{}, false, err
				}
				isNamespaced := resource.Namespaced
				return schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: resource.Name}, isNamespaced, nil
			}
		}
	}
	return schema.GroupVersionResource{}, false, fmt.Errorf("resource not found")
}
