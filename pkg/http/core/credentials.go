package core

import (
	"encoding/base64"
	"log"
	"net/http"
	"strings"
	"sync"

	clouddriver "github.com/billiford/go-clouddriver/pkg"
	"github.com/billiford/go-clouddriver/pkg/kubernetes"
	"github.com/billiford/go-clouddriver/pkg/sql"
	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// I'm not sure why spinnaker needs this, but without it several necessary Spinnaker manifest stages are missing.
// Also, All accounts with this have the same kind map, so we're hardcoding it for now.
var spinnakerKindMap = map[string]string{
	"apiService":                     "unclassified",
	"clusterRole":                    "unclassified",
	"clusterRoleBinding":             "unclassified",
	"configMap":                      "configs",
	"controllerRevision":             "unclassified",
	"cronJob":                        "serverGroups",
	"customResourceDefinition":       "unclassified",
	"daemonSet":                      "serverGroups",
	"deployment":                     "serverGroupManagers",
	"event":                          "unclassified",
	"horizontalpodautoscaler":        "unclassified",
	"ingress":                        "loadBalancers",
	"job":                            "serverGroups",
	"limitRange":                     "unclassified",
	"mutatingWebhookConfiguration":   "unclassified",
	"namespace":                      "unclassified",
	"networkPolicy":                  "securityGroups",
	"persistentVolume":               "configs",
	"persistentVolumeClaim":          "configs",
	"pod":                            "instances",
	"podDisruptionBudget":            "unclassified",
	"podPreset":                      "unclassified",
	"podSecurityPolicy":              "unclassified",
	"replicaSet":                     "serverGroups",
	"role":                           "unclassified",
	"roleBinding":                    "unclassified",
	"secret":                         "configs",
	"service":                        "loadBalancers",
	"serviceAccount":                 "unclassified",
	"statefulSet":                    "serverGroups",
	"storageClass":                   "unclassified",
	"validatingWebhookConfiguration": "unclassified",
}

// List credentials for providers.
func ListCredentials(c *gin.Context) {
	expand := c.Query("expand")
	sc := sql.Instance(c)
	kc := kubernetes.Instance(c)
	credentials := []clouddriver.Credential{}

	providers, err := sc.ListKubernetesProviders()
	if err != nil {
		clouddriver.WriteError(c, http.StatusInternalServerError, err)
		return
	}

	for _, provider := range providers {
		readGroups, err := sc.ListReadGroupsByAccountName(provider.Name)
		if err != nil {
			clouddriver.WriteError(c, http.StatusInternalServerError, err)
			return
		}

		writeGroups, err := sc.ListWriteGroupsByAccountName(provider.Name)
		if err != nil {
			clouddriver.WriteError(c, http.StatusInternalServerError, err)
			return
		}

		sca := clouddriver.Credential{
			AccountType: provider.Name,
			// CacheThreads:                0,
			// ChallengeDestructiveActions: false,
			CloudProvider: "kubernetes",
			// DockerRegistries:            nil,
			// Enabled:                     false,
			Environment: provider.Name,
			Name:        provider.Name,
			Permissions: clouddriver.Permissions{
				READ:  readGroups,
				WRITE: writeGroups,
			},
			PrimaryAccount:          false,
			ProviderVersion:         "v2",
			RequiredGroupMembership: []interface{}{},
			Skin:                    "v2",
			// SpinnakerKindMap: map[string]string{
			// 	"": "",
			// },
			Type: "kubernetes",
		}

		if expand == "true" {
			sca.SpinnakerKindMap = spinnakerKindMap
		}
		credentials = append(credentials, sca)
	}

	type AccountNamespaces struct {
		Name       string
		Namespaces []string
	}

	// Only list namespaces when the 'expand' query param is set to true.
	//
	// Gate is polling the endpoint `/credentials?expand=true` once every
	// thirty seconds. Each gate instance is doing this, making the requests to get
	// all provider's namespaces a multiple of how many gate instances there are.
	if expand == "true" {
		wg := &sync.WaitGroup{}
		accountNamespacesCh := make(chan AccountNamespaces, len(providers))
		wg.Add(len(providers))

		// Get all namespaces of allowed accounts asynchronysly.
		for _, provider := range providers {
			go func(account string) {
				defer wg.Done()

				provider, err := sc.GetKubernetesProvider(account)
				if err != nil {
					log.Println("/credentials error getting provider:", err.Error())
					return
				}

				cd, err := base64.StdEncoding.DecodeString(provider.CAData)
				if err != nil {
					log.Println("/credentials error decoding provider ca data:", err.Error())
					return
				}

				config := &rest.Config{
					Host:        provider.Host,
					BearerToken: provider.BearerToken,
					TLSClientConfig: rest.TLSClientConfig{
						CAData: cd,
					},
				}

				err = kc.SetDynamicClientForConfig(config)
				if err != nil {
					log.Println("/credentials error creating dynamic account:", err.Error())
					return
				}

				gvr := schema.GroupVersionResource{
					Group:    "",
					Version:  "v1",
					Resource: "namespaces",
				}
				// timeout listing namespaces to 5 seconds
				timeout := int64(5)
				result, err := kc.List(gvr, metav1.ListOptions{
					TimeoutSeconds: &timeout,
				})
				if err != nil {
					log.Println("/credentials error listing using kubernetes account:", err.Error())
					return
				}

				namespaces := []string{}
				for _, ns := range result.Items {
					namespaces = append(namespaces, ns.GetName())
				}
				an := AccountNamespaces{
					Name:       account,
					Namespaces: namespaces,
				}

				accountNamespacesCh <- an
			}(provider.Name)
		}

		wg.Wait()

		close(accountNamespacesCh)

		for an := range accountNamespacesCh {
			for i, cred := range credentials {
				if strings.EqualFold(an.Name, cred.Name) {
					cred.Namespaces = an.Namespaces
					credentials[i] = cred
				}
			}
		}
	}

	c.JSON(http.StatusOK, credentials)
}

func GetAccountCredentials(c *gin.Context) {
	sc := sql.Instance(c)
	account := c.Param("account")

	provider, err := sc.GetKubernetesProvider(account)
	if err != nil {
		clouddriver.WriteError(c, http.StatusInternalServerError, err)
		return
	}

	readGroups, err := sc.ListReadGroupsByAccountName(provider.Name)
	if err != nil {
		clouddriver.WriteError(c, http.StatusInternalServerError, err)
		return
	}

	writeGroups, err := sc.ListWriteGroupsByAccountName(provider.Name)
	if err != nil {
		clouddriver.WriteError(c, http.StatusInternalServerError, err)
		return
	}

	credentials := clouddriver.Credential{
		AccountType:                 provider.Name,
		ChallengeDestructiveActions: false,
		CloudProvider:               "kubernetes",
		Environment:                 provider.Name,
		Name:                        provider.Name,
		Permissions: clouddriver.Permissions{
			READ:  readGroups,
			WRITE: writeGroups,
		},
		PrimaryAccount:          false,
		ProviderVersion:         "v2",
		RequiredGroupMembership: []interface{}{},
		Skin:                    "v2",
		SpinnakerKindMap:        spinnakerKindMap,
		Type:                    "kubernetes",
	}

	c.JSON(http.StatusOK, credentials)
}