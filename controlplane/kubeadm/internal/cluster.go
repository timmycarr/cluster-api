/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package internal

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util/secret"
)

// ManagementCluster holds operations on the ManagementCluster.
type ManagementCluster struct {
	Client ctrlclient.Client
}

// GetMachinesForCluster returns a list of machines that can be filtered or not.
// If no filter is supplied then all machines associated with the target cluster are returned.
func (m *ManagementCluster) GetMachinesForCluster(ctx context.Context, cluster client.ObjectKey, filters ...MachineFilter) (FilterableMachineCollection, error) {
	selector := map[string]string{
		clusterv1.ClusterLabelName: cluster.Name,
	}
	ml := &clusterv1.MachineList{}
	if err := m.Client.List(ctx, ml, client.InNamespace(cluster.Namespace), client.MatchingLabels(selector)); err != nil {
		return nil, errors.Wrap(err, "failed to list machines")
	}

	machines := NewFilterableMachineCollectionFromMachineList(ml)
	return machines.Filter(filters...), nil
}

// GetWorkloadCluster builds a cluster object.
// The cluster comes with an etcd Client generator to connect to any etcd pod living on a managed machine.
func (m *ManagementCluster) GetWorkloadCluster(ctx context.Context, clusterKey client.ObjectKey) (*Cluster, error) {
	// TODO(chuckha): Unroll remote.NewClusterClient if we are unhappy with getting a restConfig twice.
	// TODO(chuckha): Inject this dependency.
	restConfig, err := remote.RESTConfig(ctx, m.Client, clusterKey)
	if err != nil {
		return nil, err
	}

	c, err := remote.NewClusterClient(ctx, m.Client, clusterKey, scheme.Scheme)
	if err != nil {
		return nil, err
	}
	etcdCACert, etcdCAKey, err := m.GetEtcdCerts(ctx, clusterKey)
	if err != nil {
		return nil, err
	}
	clientCert, err := generateClientCert(etcdCACert, etcdCAKey)
	if err != nil {
		return nil, err
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(etcdCACert)
	cfg := &tls.Config{
		RootCAs:      caPool,
		Certificates: []tls.Certificate{clientCert},
	}

	return &Cluster{
		Client: c,
		EtcdClientGenerator: &etcdClientGenerator{
			restConfig: restConfig,
			tlsConfig:  cfg,
		},
	}, nil
}

// GetEtcdCerts returns the EtcdCA Cert and Key for a given cluster.
func (m *ManagementCluster) GetEtcdCerts(ctx context.Context, cluster client.ObjectKey) ([]byte, []byte, error) {
	etcdCASecret := &corev1.Secret{}
	etcdCAObjectKey := client.ObjectKey{
		Namespace: cluster.Namespace,
		Name:      fmt.Sprintf("%s-etcd", cluster.Name),
	}
	if err := m.Client.Get(ctx, etcdCAObjectKey, etcdCASecret); err != nil {
		return nil, nil, errors.Wrapf(err, "failed to get secret; etcd CA bundle %s/%s", etcdCAObjectKey.Namespace, etcdCAObjectKey.Name)
	}
	crtData, ok := etcdCASecret.Data[secret.TLSCrtDataName]
	if !ok {
		return nil, nil, errors.Errorf("etcd tls crt does not exist for cluster %s/%s", cluster.Namespace, cluster.Name)
	}
	keyData, ok := etcdCASecret.Data[secret.TLSKeyDataName]
	if !ok {
		return nil, nil, errors.Errorf("etcd tls key does not exist for cluster %s/%s", cluster.Namespace, cluster.Name)
	}
	return crtData, keyData, nil
}

type healthCheck func(context.Context) (healthCheckResult, error)

// healthCheck will run a generic health check function and report any errors discovered.
// It does some additional validation to make sure there is a 1;1 match between nodes and machines.
func (m *ManagementCluster) healthCheck(ctx context.Context, check healthCheck, clusterKey client.ObjectKey, controlPlaneName string) error {
	var errorList []error
	nodeChecks, err := check(ctx)
	if err != nil {
		errorList = append(errorList, err)
	}
	for nodeName, err := range nodeChecks {
		if err != nil {
			errorList = append(errorList, fmt.Errorf("node %q: %v", nodeName, err))
		}
	}
	if len(errorList) != 0 {
		return kerrors.NewAggregate(errorList)
	}

	// Make sure Cluster API is aware of all the nodes.
	machines, err := m.GetMachinesForCluster(ctx, clusterKey, OwnedControlPlaneMachines(controlPlaneName))
	if err != nil {
		return err
	}

	// This check ensures there is a 1 to 1 correspondence of nodes and machines.
	// If a machine was not checked this is considered an error.
	for _, machine := range machines {
		if machine.Status.NodeRef == nil {
			return errors.Errorf("control plane machine %s/%s has no status.nodeRef", machine.Namespace, machine.Name)
		}
		if _, ok := nodeChecks[machine.Status.NodeRef.Name]; !ok {
			return errors.Errorf("machine's (%s/%s) node (%s) was not checked", machine.Namespace, machine.Name, machine.Status.NodeRef.Name)
		}
	}
	if len(nodeChecks) != len(machines) {
		return errors.Errorf("number of nodes and machines in namespace %s did not match: %d nodes %d machines", clusterKey.Namespace, len(nodeChecks), len(machines))
	}
	return nil
}

// TargetClusterControlPlaneIsHealthy checks every node for control plane health.
func (m *ManagementCluster) TargetClusterControlPlaneIsHealthy(ctx context.Context, clusterKey client.ObjectKey, controlPlaneName string) error {
	// TODO: add checks for expected taints/labels
	cluster, err := m.GetWorkloadCluster(ctx, clusterKey)
	if err != nil {
		return err
	}
	return m.healthCheck(ctx, cluster.controlPlaneIsHealthy, clusterKey, controlPlaneName)
}

// TargetClusterEtcdIsHealthy runs a series of checks over a target cluster's etcd cluster.
// In addition, it verifies that there are the same number of etcd members as control plane Machines.
func (m *ManagementCluster) TargetClusterEtcdIsHealthy(ctx context.Context, clusterKey client.ObjectKey, controlPlaneName string) error {
	cluster, err := m.GetWorkloadCluster(ctx, clusterKey)
	if err != nil {
		return err
	}
	return m.healthCheck(ctx, cluster.etcdIsHealthy, clusterKey, controlPlaneName)
}
