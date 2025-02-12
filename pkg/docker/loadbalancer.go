/*
Copyright 2018 The Kubernetes Authors.

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

package docker

import (
	"context"
	"fmt"
	"net"

	"github.com/pkg/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/kind/pkg/cluster/constants"

	infrav1 "github.com/beanlearninggo/cluster-api-provider-docker/api/v1alpha1"
	"github.com/beanlearninggo/cluster-api-provider-docker/pkg/container"
	"github.com/beanlearninggo/cluster-api-provider-docker/pkg/docker/types"
	"github.com/beanlearninggo/cluster-api-provider-docker/pkg/loadbalancer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

type lbCreator interface {
	CreateExternalLoadBalancerNode(ctx context.Context, name, image, clusterName, listenAddress string, port int32) (*types.Node, error)
}

// LoadBalancer manages the load balancer for a specific docker cluster.
type LoadBalancer struct {
	name      string
	image     string
	container *types.Node
	lbCreator lbCreator
}

// NewLoadBalancer returns a new helper for managing a docker loadbalancer with a given name.
func NewLoadBalancer(ctx context.Context, cluster *clusterv1.Cluster, dockerCluster *infrav1.DockerCluster) (*LoadBalancer, error) {
	if cluster.Name == "" {
		return nil, errors.New("create load balancer: cluster name is empty")
	}

	// Look for the container that is hosting the loadbalancer for the cluster.
	// Filter based on the label and the roles regardless of whether or not it is running.
	// If non-running container is chosen, then it will not have an IP address associated with it.
	filters := container.FilterBuilder{}
	filters.AddKeyNameValue(filterLabel, clusterLabelKey, cluster.Name)
	filters.AddKeyNameValue(filterLabel, nodeRoleLabelKey, constants.ExternalLoadBalancerNodeRoleValue)

	container, err := getContainer(ctx, filters)
	if err != nil {
		return nil, err
	}

	image := getLoadBalancerImage(dockerCluster)

	return &LoadBalancer{
		name:      cluster.Name,
		image:     image,
		container: container,
		lbCreator: &Manager{},
	}, nil
}

// getLoadBalancerImage will return the image (e.g. "kindest/haproxy:2.1.1-alpine") to use for
// the load balancer.
func getLoadBalancerImage(dockerCluster *infrav1.DockerCluster) string {
	// Check if a non-default image was provided
	image := loadbalancer.Image
	imageRepo := loadbalancer.DefaultImageRepository
	imageTag := loadbalancer.DefaultImageTag

	if dockerCluster != nil {
		if dockerCluster.Spec.LoadBalancerImage != "" {
			return dockerCluster.Spec.LoadBalancerImage
		}
	}

	return fmt.Sprintf("%s/%s:%s", imageRepo, image, imageTag)
}

// ContainerName is the name of the docker container with the load balancer.
func (s *LoadBalancer) containerName() string {
	return fmt.Sprintf("%s-lb", s.name)
}

// Create creates a docker container hosting a load balancer for the cluster.
func (s *LoadBalancer) Create(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)
	log = log.WithValues("loadbalancer", s.name)

	listenAddr := "0.0.0.0"

	// Create if not exists.
	if s.container == nil {
		var err error
		log.Info("Creating load balancer container")
		s.container, err = s.lbCreator.CreateExternalLoadBalancerNode(
			ctx,
			s.containerName(),
			s.image,
			s.name,
			listenAddr,
			0,
		)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// UpdateConfiguration updates the external load balancer configuration with new control plane nodes.
func (s *LoadBalancer) UpdateConfiguration(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)

	if s.container == nil {
		return errors.New("unable to configure load balancer: load balancer container does not exists")
	}

	// collect info about the existing controlplane nodes
	filters := container.FilterBuilder{}
	filters.AddKeyNameValue(filterLabel, clusterLabelKey, s.name)
	filters.AddKeyNameValue(filterLabel, nodeRoleLabelKey, constants.ControlPlaneNodeRoleValue)

	controlPlaneNodes, err := listContainers(ctx, filters)
	if err != nil {
		return errors.WithStack(err)
	}

	var backendServers = map[string]string{}
	for _, n := range controlPlaneNodes {
		controlPlaneIPv4, err := n.IP(ctx)
		if err != nil {
			return errors.Wrapf(err, "failed to get IP for container %s", n.String())
		}

		backendServers[n.String()] = net.JoinHostPort(controlPlaneIPv4, "6443")

	}

	loadBalancerConfig, err := loadbalancer.Config(&loadbalancer.ConfigData{
		ControlPlanePort: 6443,
		BackendServers:   backendServers,
		EnableStats:      true,
	})
	if err != nil {
		return errors.WithStack(err)
	}

	log.Info("Updating load balancer configuration")
	if err := s.container.WriteFile(ctx, loadbalancer.ConfigPath, loadBalancerConfig); err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(s.container.Kill(ctx, "SIGHUP"))
}

// IP returns the load balancer IP address.
func (s *LoadBalancer) IP(ctx context.Context) (string, error) {
	lbIP, err := s.container.IP(ctx)
	if err != nil {
		return "", errors.WithStack(err)
	}
	if lbIP == "" {
		// if there is a load balancer container with the same name exists but is stopped, it may not have IP address associated with it.
		return "", errors.Errorf("load balancer IP cannot be empty: container %s does not have an associated IP address", s.containerName())
	}
	return lbIP, nil
}

// Delete the docker container hosting the cluster load balancer.
func (s *LoadBalancer) Delete(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx)

	if s.container != nil {
		log.Info("Deleting load balancer container")
		if err := s.container.Delete(ctx); err != nil {
			return err
		}
		s.container = nil
	}
	return nil
}
