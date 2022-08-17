// This file has been modified to enable service account token generation
// using the RBAC v1 API. Credit to the original authors at Rancher.
// https://github.com/rancher/kontainer-engine/blob/release/v2.4/drivers/util/utils.go

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/civo/civogo"
	civoutils "github.com/civo/civogo/utils"
	"github.com/rancher/kontainer-engine/drivers/options"
	"github.com/rancher/kontainer-engine/types"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	cattleNamespace           = "cattle-system"
	clusterAdmin              = "cluster-admin"
	kontainerEngine           = "kontainer-engine"
	newClusterRoleBindingName = "system-netes-default-clusterRoleBinding"
)

func generateServiceAccountToken(clientset kubernetes.Interface) (string, error) {
	_, err := clientset.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: cattleNamespace,
		},
	}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return "", err
	}

	serviceAccount := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: kontainerEngine,
		},
	}

	_, err = clientset.CoreV1().ServiceAccounts(cattleNamespace).Create(context.TODO(), serviceAccount, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("error creating service account: %v", err)
	}

	adminRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterAdmin,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"*"},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
			{
				NonResourceURLs: []string{"*"},
				Verbs:           []string{"*"},
			},
		},
	}
	clusterAdminRole, err := clientset.RbacV1().ClusterRoles().Get(context.TODO(), clusterAdmin, metav1.GetOptions{})
	if err != nil {
		clusterAdminRole, err = clientset.RbacV1().ClusterRoles().Create(context.TODO(), adminRole, metav1.CreateOptions{})
		if err != nil {
			return "", fmt.Errorf("error creating admin role: %v", err)
		}
	}

	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: newClusterRoleBindingName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccount.Name,
				Namespace: cattleNamespace,
				APIGroup:  v1.GroupName,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     clusterAdminRole.Name,
			APIGroup: rbacv1.GroupName,
		},
	}
	if _, err = clientset.RbacV1().ClusterRoleBindings().Create(context.TODO(), clusterRoleBinding, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("error creating role bindings: %v", err)
	}

	start := time.Millisecond * 250
	for i := 0; i < 5; i++ {
		time.Sleep(start)
		if serviceAccount, err = clientset.CoreV1().ServiceAccounts(cattleNamespace).Get(context.TODO(), serviceAccount.Name, metav1.GetOptions{}); err != nil {
			return "", fmt.Errorf("error getting service account: %v", err)
		}

		if len(serviceAccount.Secrets) > 0 {
			secret := serviceAccount.Secrets[0]
			secretObj, err := clientset.CoreV1().Secrets(cattleNamespace).Get(context.TODO(), secret.Name, metav1.GetOptions{})
			if err != nil {
				return "", fmt.Errorf("error getting secret: %v", err)
			}
			if token, ok := secretObj.Data["token"]; ok {
				return string(token), nil
			}
		}
		start = start * 2
	}

	return "", fmt.Errorf("failed to fetch serviceAccountToken")
}

func generateServiceAccountTokenForCivo(kubeconfig string) (string, error) {
	result := ""

	clientset, err := civoutils.BuildClientsetFromConfig(kubeconfig, nil)
	if err != nil {
		return "", err
	}

	err = wait.Poll(retryInterval, serviceAccountRetryTimeout, func() (done bool, err error) {
		token, err := generateServiceAccountToken(clientset)
		if err != nil {
			logrus.Debugf("retrying on service account generation error: %s", err)
			return false, nil
		}

		result = token
		return true, nil
	})

	return result, err
}

// SetDriverOptions implements driver interface
func getStateFromOpts(driverOptions *types.DriverOptions) (state, error) {
	s := state{
		NodePools: map[string]string{},
		ClusterInfo: types.ClusterInfo{
			Metadata: map[string]string{},
		},
	}

	s.Name = options.GetValueFromDriverOptions(driverOptions, types.StringType, "name").(string)
	s.APIKey = options.GetValueFromDriverOptions(driverOptions, types.StringType, "api-key", "apiKey").(string)

	s.Region = options.GetValueFromDriverOptions(driverOptions, types.StringType, "region").(string)
	s.K8sVersion = options.GetValueFromDriverOptions(driverOptions, types.StringType, "kubernetes-version", "kubernetesVersion").(string)
	s.NetworkID = options.GetValueFromDriverOptions(driverOptions, types.StringType, "network-id", "networkId").(string)
	s.CNIPlugin = options.GetValueFromDriverOptions(driverOptions, types.StringType, "cni-plugin", "cniPlugin").(string)
	s.FirewallID = options.GetValueFromDriverOptions(driverOptions, types.StringType, "firewall-id", "firewallID").(string)

	pools := options.GetValueFromDriverOptions(driverOptions, types.StringSliceType, "node-pools", "nodePools")
	if pools != nil {
		for _, part := range pools.(*types.StringSlice).Value {
			kv := strings.Split(part, "=")
			if len(kv) == 2 {
				size, count, err := getSizeCountNodePool(kv[0], kv[1])
				if err != nil {
					return state{}, err
				}
				s.NodePools[kv[0]] = fmt.Sprintf("%s-%d", size, count)
			}
		}
	}

	return s, s.validate()
}

func (s *state) validate() error {
	if len(s.NodePools) == 0 {
		return fmt.Errorf("at least one NodePool is required")
	}
	for t, sc := range s.NodePools {
		_, count, err := getSizeCountNodePool(t, sc)
		if err != nil {
			return err
		}
		if count <= 0 {
			return fmt.Errorf("at least 1 node required for NodePool=%s", t)
		}
	}
	return nil
}

func storeState(info *types.ClusterInfo, state state) error {
	bytes, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if info.Metadata == nil {
		info.Metadata = map[string]string{}
	}
	info.Metadata["state"] = string(bytes)
	info.Metadata["region"] = state.Region
	return nil
}

func getState(info *types.ClusterInfo) (state, error) {
	state := state{}
	// ignore error
	err := json.Unmarshal([]byte(info.Metadata["state"]), &state)
	return state, err
}

func exists(m map[string]string, key string) bool {
	if m == nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// WaitForClusterActive waits for cluster to be active
func WaitForClusterActive(ctx context.Context, client *civogo.Client, clusterID string, timeoutSeconds int) error {
	ctx, cancel := context.WithCancel(ctx)
	if timeoutSeconds != 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	}
	defer cancel()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cluster, err := client.GetKubernetesCluster(clusterID)
			if err != nil {
				return fmt.Errorf("failed to get cluster %s: %s", clusterID, err)
			}

			if cluster.Status == "ACTIVE" {
				return nil
			}

		case <-ctx.Done():
			return fmt.Errorf("Error waiting for cluster %s to be active: %s", clusterID, ctx.Err())
		}
	}
}

func getSizeCountNodePool(uuid, part string) (string, int, error) {
	var err error
	count := 0
	size := ""
	kv := strings.Split(part, "-")
	if len(kv) == 2 {
		size = kv[0]
		count, err = strconv.Atoi(kv[1])
		if err != nil {
			return size, count, fmt.Errorf("failed to parse node count %v for pool %s of node size %s", kv[1], uuid, kv[0])
		}
	} else {
		return size, count, fmt.Errorf("failed to parse size count format")
	}

	return size, count, nil
}
