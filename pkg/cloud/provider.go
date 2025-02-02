package cloud

import (
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/opencost/opencost/pkg/cloud/aws"
	"github.com/opencost/opencost/pkg/cloud/azure"
	"github.com/opencost/opencost/pkg/cloud/gcp"
	"github.com/opencost/opencost/pkg/cloud/models"
	"github.com/opencost/opencost/pkg/kubecost"

	"github.com/opencost/opencost/pkg/util"

	"cloud.google.com/go/compute/metadata"

	"github.com/opencost/opencost/pkg/clustercache"
	"github.com/opencost/opencost/pkg/config"
	"github.com/opencost/opencost/pkg/env"
	"github.com/opencost/opencost/pkg/log"
	"github.com/opencost/opencost/pkg/util/httputil"
	"github.com/opencost/opencost/pkg/util/watcher"

	v1 "k8s.io/api/core/v1"
)

// ClusterName returns the name defined in cluster info, defaulting to the
// CLUSTER_ID environment variable
func ClusterName(p models.Provider) string {
	info, err := p.ClusterInfo()
	if err != nil {
		return env.GetClusterID()
	}

	name, ok := info["name"]
	if !ok {
		return env.GetClusterID()
	}

	return name
}

// CustomPricesEnabled returns the boolean equivalent of the cloup provider's custom prices flag,
// indicating whether or not the cluster is using custom pricing.
func CustomPricesEnabled(p models.Provider) bool {
	config, err := p.GetConfig()
	if err != nil {
		return false
	}
	// TODO:CLEANUP what is going on with this?
	if config.NegotiatedDiscount == "" {
		config.NegotiatedDiscount = "0%"
	}

	return config.CustomPricesEnabled == "true"
}

// ConfigWatcherFor returns a new ConfigWatcher instance which watches changes to the "pricing-configs"
// configmap
func ConfigWatcherFor(p models.Provider) *watcher.ConfigMapWatcher {
	return &watcher.ConfigMapWatcher{
		ConfigMapName: env.GetPricingConfigmapName(),
		WatchFunc: func(name string, data map[string]string) error {
			_, err := p.UpdateConfigFromConfigMap(data)
			return err
		},
	}
}

// AllocateIdleByDefault returns true if the application settings specify to allocate idle by default
func AllocateIdleByDefault(p models.Provider) bool {
	config, err := p.GetConfig()
	if err != nil {
		return false
	}

	return config.DefaultIdle == "true"
}

// SharedNamespace returns a list of names of shared namespaces, as defined in the application settings
func SharedNamespaces(p models.Provider) []string {
	namespaces := []string{}

	config, err := p.GetConfig()
	if err != nil {
		return namespaces
	}
	if config.SharedNamespaces == "" {
		return namespaces
	}
	// trim spaces so that "kube-system, kubecost" is equivalent to "kube-system,kubecost"
	for _, ns := range strings.Split(config.SharedNamespaces, ",") {
		namespaces = append(namespaces, strings.Trim(ns, " "))
	}

	return namespaces
}

// SharedLabel returns the configured set of shared labels as a parallel tuple of keys to values; e.g.
// for app:kubecost,type:staging this returns (["app", "type"], ["kubecost", "staging"]) in order to
// match the signature of the NewSharedResourceInfo
func SharedLabels(p models.Provider) ([]string, []string) {
	names := []string{}
	values := []string{}

	config, err := p.GetConfig()
	if err != nil {
		return names, values
	}

	if config.SharedLabelNames == "" || config.SharedLabelValues == "" {
		return names, values
	}

	ks := strings.Split(config.SharedLabelNames, ",")
	vs := strings.Split(config.SharedLabelValues, ",")
	if len(ks) != len(vs) {
		log.Warnf("Shared labels have mis-matched lengths: %d names, %d values", len(ks), len(vs))
		return names, values
	}

	for i := range ks {
		names = append(names, strings.Trim(ks[i], " "))
		values = append(values, strings.Trim(vs[i], " "))
	}

	return names, values
}

// ShareTenancyCosts returns true if the application settings specify to share
// tenancy costs by default.
func ShareTenancyCosts(p models.Provider) bool {
	config, err := p.GetConfig()
	if err != nil {
		return false
	}

	return config.ShareTenancyCosts == "true"
}

// NewProvider looks at the nodespec or provider metadata server to decide which provider to instantiate.
func NewProvider(cache clustercache.ClusterCache, apiKey string, config *config.ConfigFileManager) (models.Provider, error) {
	nodes := cache.GetAllNodes()
	if len(nodes) == 0 {
		log.Infof("Could not locate any nodes for cluster.") // valid in ETL readonly mode
		return &CustomProvider{
			Clientset: cache,
			Config:    NewProviderConfig(config, "default.json"),
		}, nil
	}

	cp := getClusterProperties(nodes[0])
	providerConfig := NewProviderConfig(config, cp.configFileName)
	// If ClusterAccount is set apply it to the cluster properties
	if providerConfig.customPricing != nil && providerConfig.customPricing.ClusterAccountID != "" {
		cp.accountID = providerConfig.customPricing.ClusterAccountID
	}

	switch cp.provider {
	case kubecost.CSVProvider:
		log.Infof("Using CSV Provider with CSV at %s", env.GetCSVPath())
		return &CSVProvider{
			CSVLocation: env.GetCSVPath(),
			CustomProvider: &CustomProvider{
				Clientset:        cache,
				clusterRegion:    cp.region,
				clusterAccountID: cp.accountID,
				Config:           NewProviderConfig(config, cp.configFileName),
			},
		}, nil
	case kubecost.GCPProvider:
		log.Info("Found ProviderID starting with \"gce\", using GCP Provider")
		if apiKey == "" {
			return nil, errors.New("Supply a GCP Key to start getting data")
		}
		return &gcp.GCP{
			Clientset:        cache,
			APIKey:           apiKey,
			Config:           NewProviderConfig(config, cp.configFileName),
			ClusterRegion:    cp.region,
			ClusterAccountID: cp.accountID,
			ClusterProjectID: cp.projectID,
			MetadataClient: metadata.NewClient(
				&http.Client{
					Transport: httputil.NewUserAgentTransport("kubecost", &http.Transport{
						Dial: (&net.Dialer{
							Timeout:   2 * time.Second,
							KeepAlive: 30 * time.Second,
						}).Dial,
					}),
					Timeout: 5 * time.Second,
				}),
		}, nil
	case kubecost.AWSProvider:
		log.Info("Found ProviderID starting with \"aws\", using AWS Provider")
		return &aws.AWS{
			Clientset:            cache,
			Config:               NewProviderConfig(config, cp.configFileName),
			ClusterRegion:        cp.region,
			ClusterAccountID:     cp.accountID,
			ServiceAccountChecks: models.NewServiceAccountChecks(),
		}, nil
	case kubecost.AzureProvider:
		log.Info("Found ProviderID starting with \"azure\", using Azure Provider")
		return &azure.Azure{
			Clientset:            cache,
			Config:               NewProviderConfig(config, cp.configFileName),
			ClusterRegion:        cp.region,
			ClusterAccountID:     cp.accountID,
			ServiceAccountChecks: models.NewServiceAccountChecks(),
		}, nil
	case kubecost.AlibabaProvider:
		log.Info("Found ProviderID starting with \"alibaba\", using Alibaba Cloud Provider")
		return &Alibaba{
			Clientset:            cache,
			Config:               NewProviderConfig(config, cp.configFileName),
			clusterRegion:        cp.region,
			clusterAccountId:     cp.accountID,
			serviceAccountChecks: models.NewServiceAccountChecks(),
		}, nil
	case kubecost.ScalewayProvider:
		log.Info("Found ProviderID starting with \"scaleway\", using Scaleway Provider")
		return &Scaleway{
			Clientset:        cache,
			clusterRegion:    cp.region,
			clusterAccountID: cp.accountID,
			Config:           NewProviderConfig(config, cp.configFileName),
		}, nil

	default:
		log.Info("Unsupported provider, falling back to default")
		return &CustomProvider{
			Clientset:        cache,
			clusterRegion:    cp.region,
			clusterAccountID: cp.accountID,
			Config:           NewProviderConfig(config, cp.configFileName),
		}, nil
	}
}

type clusterProperties struct {
	provider       string
	configFileName string
	region         string
	accountID      string
	projectID      string
}

func getClusterProperties(node *v1.Node) clusterProperties {
	providerID := strings.ToLower(node.Spec.ProviderID)
	region, _ := util.GetRegion(node.Labels)
	cp := clusterProperties{
		provider:       "DEFAULT",
		configFileName: "default.json",
		region:         region,
		accountID:      "",
		projectID:      "",
	}
	// The second conditional is mainly if you're running opencost outside of GCE, say in a local environment.
	if metadata.OnGCE() || strings.HasPrefix(providerID, "gce") {
		cp.provider = kubecost.GCPProvider
		cp.configFileName = "gcp.json"
		cp.projectID = gcp.ParseGCPProjectID(providerID)
	} else if strings.HasPrefix(providerID, "aws") {
		cp.provider = kubecost.AWSProvider
		cp.configFileName = "aws.json"
	} else if strings.HasPrefix(providerID, "azure") {
		cp.provider = kubecost.AzureProvider
		cp.configFileName = "azure.json"
		cp.accountID = azure.ParseAzureSubscriptionID(providerID)
	} else if strings.HasPrefix(providerID, "scaleway") { // the scaleway provider ID looks like scaleway://instance/<instance_id>
		cp.provider = kubecost.ScalewayProvider
		cp.configFileName = "scaleway.json"
	} else if strings.Contains(node.Status.NodeInfo.KubeletVersion, "aliyun") { // provider ID is not prefix with any distinct keyword like other providers
		cp.provider = kubecost.AlibabaProvider
		cp.configFileName = "alibaba.json"
	}
	if env.IsUseCSVProvider() {
		cp.provider = kubecost.CSVProvider
	}

	return cp
}

var (
	// It's of the form aws:///us-east-2a/i-0fea4fd46592d050b and we want i-0fea4fd46592d050b, if it exists
	providerAWSRegex = regexp.MustCompile("aws://[^/]*/[^/]*/([^/]+)")
	// gce://guestbook-227502/us-central1-a/gke-niko-n1-standard-2-wljla-8df8e58a-hfy7
	//  => gke-niko-n1-standard-2-wljla-8df8e58a-hfy7
	providerGCERegex = regexp.MustCompile("gce://[^/]*/[^/]*/([^/]+)")
	// Capture "vol-0fc54c5e83b8d2b76" from "aws://us-east-2a/vol-0fc54c5e83b8d2b76"
	persistentVolumeAWSRegex = regexp.MustCompile("aws:/[^/]*/[^/]*/([^/]+)")
	// Capture "ad9d88195b52a47c89b5055120f28c58" from "ad9d88195b52a47c89b5055120f28c58-1037804914.us-east-2.elb.amazonaws.com"
	loadBalancerAWSRegex = regexp.MustCompile("^([^-]+)-.+amazonaws\\.com$")
)

// ParseID attempts to parse a ProviderId from a string based on formats from the various providers and
// returns the string as is if it cannot find a match
func ParseID(id string) string {
	match := providerAWSRegex.FindStringSubmatch(id)
	if len(match) >= 2 {
		return match[1]
	}

	match = providerGCERegex.FindStringSubmatch(id)
	if len(match) >= 2 {
		return match[1]
	}

	// Return id for Azure Provider, CSV Provider and Custom Provider
	return id
}

// ParsePVID attempts to parse a PV ProviderId from a string based on formats from the various providers and
// returns the string as is if it cannot find a match
func ParsePVID(id string) string {
	match := persistentVolumeAWSRegex.FindStringSubmatch(id)
	if len(match) >= 2 {
		return match[1]
	}

	// Return id for GCP Provider, Azure Provider, CSV Provider and Custom Provider
	return id
}

// ParseLBID attempts to parse a LB ProviderId from a string based on formats from the various providers and
// returns the string as is if it cannot find a match
func ParseLBID(id string) string {
	match := loadBalancerAWSRegex.FindStringSubmatch(id)
	if len(match) >= 2 {
		return match[1]
	}

	// Return id for GCP Provider, Azure Provider, CSV Provider and Custom Provider
	return id
}
