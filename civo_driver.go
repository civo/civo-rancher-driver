package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"time"

	"github.com/rancher/kontainer-engine/types"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/civo/civogo"
)

// DefaultCivoURL is the production API URL to use
const DefaultCivoURL = "https://api.civo.com"
const retryInterval = 5 * time.Second
const serviceAccountRetryTimeout = 5 * time.Minute

// Driver defines the struct of Civo driver
type Driver struct {
	driverCapabilities types.Capabilities
}

type state struct {
	APIKey string

	// The name of this cluster
	Name string

	// The region to launch the cluster
	Region string
	// The kubernetes version
	K8sVersion string
	NodePools  map[string]string // uuid -> size-count

	// The network ID to use for the cluster
	NetworkID string

	// The CNI plugin to use for the cluster
	CNIPlugin string

	// Firewall ID to apply to the cluster
	FirewallID string

	// cluster info
	ClusterInfo types.ClusterInfo
}

// NewDriver creates a new driver
func NewDriver() types.Driver {
	driver := &Driver{
		driverCapabilities: types.Capabilities{
			Capabilities: make(map[int64]bool),
		},
	}

	driver.driverCapabilities.AddCapability(types.GetVersionCapability)
	driver.driverCapabilities.AddCapability(types.SetVersionCapability)
	driver.driverCapabilities.AddCapability(types.GetClusterSizeCapability)
	driver.driverCapabilities.AddCapability(types.SetClusterSizeCapability)

	return driver
}

// GetDriverCreateOptions implements driver interface
func (d *Driver) GetDriverCreateOptions(ctx context.Context) (*types.DriverFlags, error) {
	driverFlag := types.DriverFlags{
		Options: make(map[string]*types.Flag),
	}

	driverFlag.Options["api-key"] = &types.Flag{
		Type:  types.StringType,
		Usage: "Civo API key to manage clusters",
	}

	driverFlag.Options["name"] = &types.Flag{
		Type:  types.StringType,
		Usage: "the internal name of the cluster in Rancher",
	}

	driverFlag.Options["region"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The region to launch the cluster. Defaults to LON1",
		Default: &types.Default{
			DefaultString: "LON1",
		},
	}

	driverFlag.Options["kubernetes-version"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The kubernetes version. Defaults to the latest stable version.",
	}

	driverFlag.Options["node-pools"] = &types.Flag{
		Type:  types.StringSliceType,
		Usage: "The list of node pools created for the cluster. Example: 'node-pools: [\"g3.small: 2\", \"g3.medium: 1\"]'",
	}

	driverFlag.Options["network-id"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The network ID to use for the cluster. By default, default network is used",
		Default: &types.Default{
			DefaultString: "default",
		},
	}

	driverFlag.Options["cni-plugin"] = &types.Flag{
		Type:  types.StringType,
		Usage: "The CNI plugin to use for the cluster. By default, flannel is used",
		Default: &types.Default{
			DefaultString: "flannel",
		},
	}

	driverFlag.Options["firewall-id"] = &types.Flag{
		Type:  types.StringType,
		Usage: "Firewall ID to apply to the cluster. By default: 80,443,6443 ingress will be allowed",
	}

	return &driverFlag, nil
}

// GetDriverUpdateOptions implements driver interface
func (d *Driver) GetDriverUpdateOptions(ctx context.Context) (*types.DriverFlags, error) {
	driverFlag := types.DriverFlags{
		Options: make(map[string]*types.Flag),
	}

	driverFlag.Options["node-pools"] = &types.Flag{
		Type:  types.StringSliceType,
		Usage: "The list of node pools created for the cluster",
	}

	return &driverFlag, nil
}

// Create  creates the cluster in the managed Kubernetes provider and populates sufficient information on the ClusterInfo return value so that PostCheck can connect to the cluster.
func (d *Driver) Create(ctx context.Context, opts *types.DriverOptions, _ *types.ClusterInfo) (*types.ClusterInfo, error) {
	state, err := getStateFromOpts(opts)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("state.name %s, state: %#v", state.Name, state)

	info := &types.ClusterInfo{}
	err = storeState(info, state)
	if err != nil {
		return info, err
	}

	client, err := d.getServiceClient(state.APIKey, state.Region)
	if err != nil {
		return info, err
	}

	req, err := d.generateClusterCreateRequest(state)
	if err != nil {
		return nil, fmt.Errorf("failed to create cluster: %s", err)
	}
	logrus.Debugf("Civo api request: %#v", req)

	cluster, err := client.NewKubernetesClusters(req)
	if err != nil {
		return nil, fmt.Errorf("failed to create Civo cluster: %s", err)
	}
	info.Metadata["cluster-id"] = cluster.ID

	err = WaitForClusterActive(ctx, client, cluster.ID, 300)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for cluster to be active: %s", err)
	}

	return info, nil
}

// Update updates the cluster in the managed Kubernetes. Like Create it must ensure ClusterInfo is populated with sufficient information for PostCheck to generate a service account token. If no connection information has changed, then it can be left alone and it will reuse the information generated by Create.
func (d *Driver) Update(ctx context.Context, info *types.ClusterInfo, opts *types.DriverOptions) (*types.ClusterInfo, error) {
	state, err := getState(info)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("state.name %s, state: %#v", state.Name, state)

	newState, err := getStateFromOpts(opts)
	if err != nil {
		return nil, err
	}

	state.APIKey = newState.APIKey

	client, err := d.getServiceClient(state.APIKey, state.Region)
	if err != nil {
		return info, err
	}

	var shouldUpdate bool
	updateOpts := &civogo.KubernetesClusterConfig{}
	if newState.FirewallID != "" && newState.FirewallID != state.FirewallID {
		shouldUpdate = true
		updateOpts.InstanceFirewall = newState.FirewallID
	}

	if newState.K8sVersion != "" && newState.K8sVersion != state.K8sVersion {
		shouldUpdate = true
		updateOpts.KubernetesVersion = newState.K8sVersion
	}

	if newState.NodePools != nil && !reflect.DeepEqual(newState.NodePools, state.NodePools) {
		shouldUpdate = true
		for t, sc := range newState.NodePools {
			size, count, err := getSizeCountNodePool(t, sc)
			if err != nil {
				return nil, err
			}
			updateOpts.Pools = append(updateOpts.Pools, civogo.KubernetesClusterPoolConfig{
				ID:    t,
				Size:  size,
				Count: count,
			})
		}
	}

	clusterID := info.Metadata["cluster-id"]
	if shouldUpdate {
		_, err = client.UpdateKubernetesCluster(clusterID, &civogo.KubernetesClusterConfig{
			InstanceFirewall: newState.FirewallID,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to update cluster %s: %s", clusterID, err)
		}
	}

	return info, storeState(info, state)
}

func (d *Driver) generateClusterCreateRequest(state state) (*civogo.KubernetesClusterConfig, error) {
	req := civogo.KubernetesClusterConfig{
		Name:              state.Name,
		Region:            state.Region,
		KubernetesVersion: state.K8sVersion,
		NetworkID:         state.NetworkID,
		CNIPlugin:         state.CNIPlugin,
		InstanceFirewall:  state.FirewallID,
		// Apps?
	}
	for t, sc := range state.NodePools {
		size, count, err := getSizeCountNodePool(t, sc)
		if err != nil {
			return nil, err
		}
		req.Pools = append(req.Pools, civogo.KubernetesClusterPoolConfig{
			ID:    t,
			Size:  size,
			Count: count,
		})
	}

	return &req, nil
}

// PostCheck : This func must populate the ClusterInfo.serviceAccountToken field with a service account token with sufficient permissions for Rancher to manage the cluster.
func (d *Driver) PostCheck(ctx context.Context, info *types.ClusterInfo) (*types.ClusterInfo, error) {
	state, err := getState(info)
	if err != nil {
		return nil, err
	}

	var kubeconfig string
	if exists(info.Metadata, "KubeConfig") {
		kubeconfig = info.Metadata["KubeConfig"]
	} else {
		// Only load Kubeconfig during first run
		client, err := d.getServiceClient(state.APIKey, state.Region)
		if err != nil {
			return nil, err
		}

		clusterID := info.Metadata["cluster-id"]

		err = WaitForClusterActive(ctx, client, clusterID, 300)
		if err != nil {
			return nil, fmt.Errorf("cluster wasn't ready in 5mins %s: %s", clusterID, err)
		}

		cluster, err := client.GetKubernetesCluster(clusterID)
		if err != nil {
			return nil, fmt.Errorf("failed to get cluster %s: %s", clusterID, err)
		}
		kubeconfig = cluster.KubeConfig
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("failed to parse cluster kubeconfig: %s", err)
	}

	info.Version = state.K8sVersion
	count := 0
	for uuid, sc := range state.NodePools {
		_, cnt, err := getSizeCountNodePool(uuid, sc)
		if err != nil {
			return nil, err
		}
		count += cnt
	}
	info.NodeCount = int64(count)

	info.Endpoint = cfg.Host
	info.Username = cfg.Username
	info.Password = cfg.Password
	if len(cfg.CAData) > 0 {
		info.RootCaCertificate = base64.StdEncoding.EncodeToString(cfg.CAData)
	}
	if len(cfg.CertData) > 0 {
		info.ClientCertificate = base64.StdEncoding.EncodeToString(cfg.CertData)
	}
	if len(cfg.KeyData) > 0 {
		info.ClientKey = base64.StdEncoding.EncodeToString(cfg.KeyData)
	}

	info.Metadata["KubeConfig"] = kubeconfig
	serviceAccountToken, err := generateServiceAccountTokenForCivo(kubeconfig)
	if err != nil {
		return nil, err
	}
	info.ServiceAccountToken = serviceAccountToken
	return info, nil
}

// Remove removes the cluster from the cloud provider.
func (d *Driver) Remove(ctx context.Context, info *types.ClusterInfo) error {
	state, err := getState(info)
	if err != nil {
		return err
	}

	client, err := d.getServiceClient(state.APIKey, state.Region)
	if err != nil {
		return err
	}

	clusterID := info.Metadata["cluster-id"]

	logrus.Debugf("Removing cluster %v from region %v", state.Name, state.Region)

	_, err = client.DeleteKubernetesCluster(clusterID)
	if err != nil {
		return fmt.Errorf("failed to delete cluster %s: %s", clusterID, err)
	}

	return nil
}

func (d *Driver) getServiceClient(key, region string) (*civogo.Client, error) {
	client, err := civogo.NewClientWithURL(key, DefaultCivoURL, region)
	if err != nil {
		return nil, err
	}

	return client, nil
}

// GetClusterSize returns the size of the cluster.
func (d *Driver) GetClusterSize(ctx context.Context, info *types.ClusterInfo) (*types.NodeCount, error) {
	state, err := getState(info)
	if err != nil {
		return nil, err
	}

	clusterID := info.Metadata["cluster-id"]

	client, err := d.getServiceClient(state.APIKey, state.Region)
	if err != nil {
		return nil, err
	}

	cluster, err := client.GetKubernetesCluster(clusterID)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster %s: %s", clusterID, err)
	}

	count := 0
	for _, pool := range cluster.Pools {
		count += pool.Count
	}
	return &types.NodeCount{Count: int64(count)}, nil
}

// GetVersion returns the version of the driver.
func (d *Driver) GetVersion(ctx context.Context, info *types.ClusterInfo) (*types.KubernetesVersion, error) {
	state, err := getState(info)
	if err != nil {
		return nil, err
	}

	clusterID := info.Metadata["cluster-id"]
	client, err := d.getServiceClient(state.APIKey, state.Region)
	if err != nil {
		return nil, err
	}

	cluster, err := client.GetKubernetesCluster(clusterID)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster %s: %s", clusterID, err)
	}

	return &types.KubernetesVersion{Version: cluster.KubernetesVersion}, nil
}

// SetClusterSize sets the size of the cluster.
func (d *Driver) SetClusterSize(ctx context.Context, info *types.ClusterInfo, count *types.NodeCount) error {
	state, err := getState(info)
	if err != nil {
		return err
	}

	clusterID := info.Metadata["cluster-id"]

	client, err := d.getServiceClient(state.APIKey, state.Region)
	if err != nil {
		return err
	}

	logrus.Info("updating cluster size")

	cluster, err := client.GetKubernetesCluster(clusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster %s: %s", clusterID, err)
	}

	poolID := cluster.Pools[0].ID
	poolSize := cluster.Pools[0].Size

	_, err = client.UpdateKubernetesClusterPool(clusterID, poolID, &civogo.KubernetesClusterPoolUpdateConfig{
		ID:     poolID,
		Count:  int(count.Count),
		Region: state.Region,
		Size:   poolSize,
	})

	logrus.Info("cluster size updated successfully")

	return nil
}

// SetVersion updates the version of the cluster.
func (d *Driver) SetVersion(ctx context.Context, info *types.ClusterInfo, version *types.KubernetesVersion) error {
	return nil
}

// GetCapabilities returns the capabilities of the driver.
func (d *Driver) GetCapabilities(ctx context.Context) (*types.Capabilities, error) {
	return &d.driverCapabilities, nil
}

// ETCDSave is not implemented yet
func (d *Driver) ETCDSave(ctx context.Context, clusterInfo *types.ClusterInfo, opts *types.DriverOptions, snapshotName string) error {
	return fmt.Errorf("ETCD backup operations are not implemented")
}

// ETCDRestore isn't implemented yet.
func (d *Driver) ETCDRestore(ctx context.Context, clusterInfo *types.ClusterInfo, opts *types.DriverOptions, snapshotName string) (*types.ClusterInfo, error) {
	return nil, fmt.Errorf("ETCD backup operations are not implemented")
}

// ETCDRemoveSnapshot isn't implemented yet.
func (d *Driver) ETCDRemoveSnapshot(ctx context.Context, clusterInfo *types.ClusterInfo, opts *types.DriverOptions, snapshotName string) error {
	return fmt.Errorf("ETCD backup operations are not implemented")
}

// GetK8SCapabilities returns the k8s capabilities of the driver.
func (d *Driver) GetK8SCapabilities(ctx context.Context, options *types.DriverOptions) (*types.K8SCapabilities, error) {
	capabilities := &types.K8SCapabilities{
		L4LoadBalancer: &types.LoadBalancerCapabilities{
			Enabled:              true,
			Provider:             "NodeBalancer", // what are the options?
			ProtocolsSupported:   []string{"TCP", "UDP"},
			HealthCheckSupported: true,
		},
	}
	return capabilities, nil
}

// RemoveLegacyServiceAccount removes the legacy service account from the cluster.
func (d *Driver) RemoveLegacyServiceAccount(ctx context.Context, info *types.ClusterInfo) error {
	return nil
}
