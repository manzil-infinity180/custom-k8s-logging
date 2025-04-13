package logs

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
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
	//tweakListOptions := nil

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
			labelsOld := old.GetLabels()
			labelsNew := new.GetLabels()
			if !reflect.DeepEqual(labelsOld, labelsNew) {
				labelMsg := fmt.Sprintf("[%s] LABELS CHANGED: %v → %v", timestamp, labelsOld, labelsNew)
				if err := conn.WriteMessage(websocket.TextMessage, []byte(labelMsg)); err != nil {
					log.Println("Error writing label change to WebSocket:", err)
				}
			}

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
				oldTypes, _, err1 := unstructured.NestedString(old.Object, "spec", "types")
				newTypes, _, err2 := unstructured.NestedString(new.Object, "spec", "types")

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
