package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/civo/civogo"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	"github.com/rancher/kontainer-engine/types"
)

func TestDriver(t *testing.T) {
	t.Parallel()

	name := generateResourceName()
	g := NewGomegaWithT(t)

	key := getCivoAPIKey(g)

	d := &Driver{}
	client, err := d.getServiceClient(key, "LON1")
	g.Expect(err).NotTo(HaveOccurred())

	kubernetesVersion := getLatestK8sVersion(t, client)

	network, err := client.GetDefaultNetwork()
	g.Expect(err).NotTo(HaveOccurred())

	opts := types.DriverOptions{
		BoolOptions: nil,
		StringOptions: map[string]string{
			"name":               name,
			"region":             "LON1",
			"kubernetes-version": kubernetesVersion,
			"network-id":         network.ID,
			"api-key":            key,
		},
		StringSliceOptions: map[string]*types.StringSlice{
			"node-pools": {
				Value: []string{
					"g3.medium=3",
				},
			},
		},
	}
	info, err := d.Create(context.Background(), &opts, nil)
	g.Expect(err).NotTo(HaveOccurred())

	defer func() {
		err = d.Remove(context.Background(), info)
		g.Expect(err).NotTo(HaveOccurred())
	}()

	g.Eventually(clusterStatusFunc(g, client, info.Metadata["cluster-id"]), "3m", "5s").Should(Equal("ACTIVE"))

	info, err = d.PostCheck(context.Background(), info)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(info.Metadata["KubeConfig"]).NotTo(BeEmpty())
	v, err := d.GetVersion(context.Background(), info)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(v.Version).To(Equal(kubernetesVersion))

	c, err := d.GetClusterSize(context.Background(), info)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c.Count).To(Equal(int64(3)))
}

func clusterStatusFunc(g *WithT, client *civogo.Client, clusterID string) func() string {
	return func() string {
		cluster, err := client.GetKubernetesCluster(clusterID)
		g.Expect(err).NotTo(HaveOccurred())
		return cluster.Status
	}
}

func getLatestK8sVersion(t *testing.T, client *civogo.Client) string {
	versions, err := client.ListAvailableKubernetesVersions()
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range versions {
		if v.Default == true {
			return v.Version
		}
	}

	return ""
}

func getCivoAPIKey(g *WithT) string {
	token := os.Getenv("CIVO_API_KEY")
	if token == "" {
		g.Fail("CIVO_API_KEY not set")
	}

	return token
}

func generateResourceName() string {
	return "civo-e2e-test-" + strings.Replace(uuid.New().String(), "-", "", -1)[0:15]
}
