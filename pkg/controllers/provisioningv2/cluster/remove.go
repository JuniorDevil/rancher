package cluster

import (
	"fmt"
	"sort"
	"time"

	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	"github.com/rancher/wrangler/pkg/condition"
	"github.com/rancher/wrangler/pkg/generic"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	capiClusterLabel = "cluster.x-k8s.io/cluster-name"
)

var (
	Removed = condition.Cond("Removed")
)

func (h *handler) OnMgmtClusterRemove(key string, cluster *v3.Cluster) (*v3.Cluster, error) {
	provisioningClusters, err := h.clusterCache.GetByIndex(ByCluster, cluster.Name)
	if err != nil {
		return nil, err
	}
	for _, provisioningCluster := range provisioningClusters {
		if err := h.clusters.Delete(provisioningCluster.Namespace, provisioningCluster.Name, nil); err != nil {
			return nil, err
		}
	}

	return cluster, nil
}

func (h *handler) updateClusterStatus(cluster *v1.Cluster, status v1.ClusterStatus, previousErr error) (*v1.Cluster, error) {
	if equality.Semantic.DeepEqual(status, cluster.Status) {
		return cluster, previousErr
	}
	cluster = cluster.DeepCopy()
	cluster.Status = status
	cluster, err := h.clusters.UpdateStatus(cluster)
	if err != nil {
		return cluster, err
	}
	return cluster, previousErr
}

func (h *handler) OnClusterRemove(_ string, cluster *v1.Cluster) (*v1.Cluster, error) {
	status := cluster.Status.DeepCopy()
	message, err := h.doClusterRemove(cluster)
	if err != nil {
		Removed.SetError(status, "", err)
	} else if message == "" {
		Removed.SetStatusBool(status, true)
		Removed.Reason(status, "")
		Removed.Message(status, "")
	} else {
		Removed.SetStatus(status, "Unknown")
		Removed.Reason(status, "Waiting")
		Removed.Message(status, message)
		h.clusters.EnqueueAfter(cluster.Namespace, cluster.Name, 5*time.Second)
		// generic.ErrSkip will mark the cluster as reconciled, but not remove the finalizer.
		// The finalizer shouldn't be removed until other objects have all been removed.
		err = generic.ErrSkip
	}
	return h.updateClusterStatus(cluster, *status, err)
}

func (h *handler) doClusterRemove(cluster *v1.Cluster) (string, error) {
	if cluster.Status.ClusterName != "" {
		err := h.mgmtClusters.Delete(cluster.Status.ClusterName, nil)
		if err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}

		_, err = h.mgmtClusterCache.Get(cluster.Status.ClusterName)
		if !apierrors.IsNotFound(err) {
			return fmt.Sprintf("waiting for cluster [%s] to delete", cluster.Status.ClusterName), nil
		}
	}

	capiCluster, capiClusterErr := h.capiClustersCache.Get(cluster.Namespace, cluster.Name)
	if capiClusterErr != nil && !apierrors.IsNotFound(capiClusterErr) {
		return "", capiClusterErr
	}

	if capiCluster != nil && capiCluster.DeletionTimestamp == nil {
		// Deleting the CAPI cluster will start the process of deleting Machines, Bootstraps, etc.
		if err := h.capiClusters.Delete(capiCluster.Namespace, capiCluster.Name, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
	}

	// Machines will delete first so report their status, if any exist.
	machines, err := h.capiMachinesCache.List(cluster.Namespace, labels.SelectorFromSet(labels.Set{
		capiClusterLabel: cluster.Name,
	}))
	if err != nil {
		return "", err
	}
	sort.Slice(machines, func(i, j int) bool {
		return machines[i].Name < machines[j].Name
	})
	for _, machine := range machines {
		return fmt.Sprintf("waiting for machine [%s] to delete", machine.Name), nil
	}

	if capiClusterErr == nil {
		return fmt.Sprintf("waiting for cluster-api cluster [%s] to delete", cluster.Name), nil
	}

	return "", nil
}
