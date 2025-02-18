// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package ip

import (
	"fmt"
	"sync"

	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/api"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/aws/ec2"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/aws/vpc"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/config"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/pool"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/provider"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/provider/ip/eni"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/worker"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
)

type ipv4Provider struct {
	// log is the logger initialized with ip provider details
	log logr.Logger
	// apiWrapper wraps all clients used by the controller
	apiWrapper api.Wrapper
	// workerPool with worker routine to execute asynchronous job on the ip provider
	workerPool worker.Worker
	// config is the warm pool configuration for the resource IPv4
	config *config.WarmPoolConfig
	// lock to allow multiple routines to access the cache concurrently
	lock sync.RWMutex // guards the following
	// instanceResources stores the ENIManager and the resource pool per instance
	instanceProviderAndPool map[string]ResourceProviderAndPool
}

// InstanceResource contains the instance's ENI manager and the resource pool
type ResourceProviderAndPool struct {
	eniManager   eni.ENIManager
	resourcePool pool.Pool
}

func NewIPv4Provider(log logr.Logger, apiWrapper api.Wrapper,
	workerPool worker.Worker, resourceConfig config.ResourceConfig) provider.ResourceProvider {
	return &ipv4Provider{
		instanceProviderAndPool: make(map[string]ResourceProviderAndPool),
		config:                  resourceConfig.WarmPoolConfig,
		log:                     log,
		apiWrapper:              apiWrapper,
		workerPool:              workerPool,
	}
}

func (p *ipv4Provider) InitResource(instance ec2.EC2Instance) error {
	nodeName := instance.Name()

	eniManager := eni.NewENIManager(instance)
	presentIPs, err := eniManager.InitResources(p.apiWrapper.EC2API)
	if err != nil {
		return err
	}

	pods, err := p.apiWrapper.PodAPI.GetRunningPodsOnNode(nodeName)
	if err != nil {
		return err
	}

	podToResourceMap := map[string]string{}
	usedIPSet := map[string]struct{}{}
	for _, pod := range pods {
		annotation, present := pod.Annotations[config.ResourceNameIPAddress]
		if !present {
			continue
		}
		podToResourceMap[string(pod.UID)] = annotation
		usedIPSet[annotation] = struct{}{}
	}

	warmResources := difference(presentIPs, usedIPSet)

	nodeCapacity := getCapacity(instance.Type(), instance.Os())
	resourcePool := pool.NewResourcePool(p.log.WithName("ipv4 resource pool").
		WithValues("node name", instance.Name()), p.config, podToResourceMap,
		warmResources, instance.Name(), nodeCapacity)

	p.putInstanceProviderAndPool(nodeName, resourcePool, eniManager)

	p.log.Info("initialized the resource provider for resource IPv4",
		"capacity", nodeCapacity, "node name", nodeName, "instance type",
		instance.Type(), "instance ID", instance.InstanceID())

	// Reconcile pool after starting up and submit the async job
	job := resourcePool.ReconcilePool()
	if job.Operations != worker.OperationReconcileNotRequired {
		p.SubmitAsyncJob(job)
	}

	// Submit the async job to periodically process the delete queue
	p.SubmitAsyncJob(worker.NewWarmProcessDeleteQueueJob(nodeName))
	return nil
}

func (p *ipv4Provider) DeInitResource(instance ec2.EC2Instance) error {
	nodeName := instance.Name()
	p.deleteInstanceProviderAndPool(nodeName)

	return nil
}

// UpdateResourceCapacity updates the resource capacity based on the type of instance
func (p *ipv4Provider) UpdateResourceCapacity(instance ec2.EC2Instance) error {
	instanceType := instance.Type()
	instanceName := instance.Name()
	os := instance.Os()

	capacity := getCapacity(instanceType, os)

	err := p.apiWrapper.K8sAPI.AdvertiseCapacityIfNotSet(instance.Name(), config.ResourceNameIPAddress, capacity)
	if err != nil {
		return err
	}
	p.log.V(1).Info("advertised capacity",
		"instance", instanceName, "instance type", instanceType, "os", os, "capacity", capacity)

	return nil
}

func (p *ipv4Provider) ProcessDeleteQueue(job *worker.WarmPoolJob) (ctrl.Result, error) {
	resourceProviderAndPool, isPresent := p.getInstanceProviderAndPool(job.NodeName)
	if !isPresent {
		p.log.Info("forgetting the delete queue processing job", "node", job.NodeName)
		return ctrl.Result{}, nil
	}
	// TODO: For efficiency run only when required in next release
	resourceProviderAndPool.resourcePool.ProcessCoolDownQueue()

	// After the cool down queue is processed check if we need to do reconciliation
	job = resourceProviderAndPool.resourcePool.ReconcilePool()
	if job.Operations != worker.OperationReconcileNotRequired {
		p.SubmitAsyncJob(job)
	}

	// Re submit the job to execute after cool down period has ended
	return ctrl.Result{Requeue: true, RequeueAfter: config.CoolDownPeriod}, nil
}

// SubmitAsyncJob submits an asynchronous job to the worker pool
func (p *ipv4Provider) SubmitAsyncJob(job interface{}) {
	p.workerPool.SubmitJob(job)
}

// ProcessAsyncJob processes the job, the function should be called using the worker pool in order to be processed
// asynchronously
func (p *ipv4Provider) ProcessAsyncJob(job interface{}) (ctrl.Result, error) {
	warmPoolJob, isValid := job.(*worker.WarmPoolJob)
	if !isValid {
		return ctrl.Result{}, fmt.Errorf("invalid job type")
	}

	switch warmPoolJob.Operations {
	case worker.OperationCreate:
		p.CreatePrivateIPv4AndUpdatePool(warmPoolJob)
	case worker.OperationDeleted:
		p.DeletePrivateIPv4AndUpdatePool(warmPoolJob)
	case worker.OperationReSyncPool:
		p.ReSyncPool(warmPoolJob)
	case worker.OperationProcessDeleteQueue:
		return p.ProcessDeleteQueue(warmPoolJob)
	}

	return ctrl.Result{}, nil
}

// CreatePrivateIPv4AndUpdatePool executes the Create IPv4 workflow by assigning the desired number of IPv4 address
// provided in the warm pool job
func (p *ipv4Provider) CreatePrivateIPv4AndUpdatePool(job *worker.WarmPoolJob) {
	instanceResource, found := p.getInstanceProviderAndPool(job.NodeName)
	if !found {
		p.log.Error(fmt.Errorf("cannot find the instance provider and pool form the cache"), "node", job.NodeName)
		return
	}
	didSucceed := true
	ips, err := instanceResource.eniManager.CreateIPV4Address(job.ResourceCount, p.apiWrapper.EC2API, p.log)
	if err != nil {
		p.log.Error(err, "failed to create all/some of the IPv4 addresses", "created ips", ips)
		didSucceed = false
	}
	job.Resources = ips
	p.updatePoolAndReconcileIfRequired(instanceResource.resourcePool, job, didSucceed)
}

func (p *ipv4Provider) ReSyncPool(job *worker.WarmPoolJob) {
	providerAndPool, found := p.instanceProviderAndPool[job.NodeName]
	if !found {
		p.log.Error(fmt.Errorf("instance provider not found"), "node is not initialized",
			"name", job.NodeName)
		return
	}

	resources, err := providerAndPool.eniManager.InitResources(p.apiWrapper.EC2API)
	if err != nil {
		p.log.Error(err, "failed to get init resources for the node",
			"name", job.NodeName)
		return
	}

	providerAndPool.resourcePool.ReSync(resources)
}

// DeletePrivateIPv4AndUpdatePool executes the Delete IPv4 workflow for the list of IPs provided in the warm pool job
func (p *ipv4Provider) DeletePrivateIPv4AndUpdatePool(job *worker.WarmPoolJob) {
	instanceResource, found := p.getInstanceProviderAndPool(job.NodeName)
	if !found {
		p.log.Error(fmt.Errorf("cannot find the instance provider and pool form the cache"), "node", job.NodeName)
		return
	}
	didSucceed := true
	failedIPs, err := instanceResource.eniManager.DeleteIPV4Address(job.Resources, p.apiWrapper.EC2API, p.log)
	if err != nil {
		p.log.Error(err, "failed to delete all/some of the IPv4 addresses", "failed ips", failedIPs)
		didSucceed = false
	}
	job.Resources = failedIPs
	p.updatePoolAndReconcileIfRequired(instanceResource.resourcePool, job, didSucceed)
}

// updatePoolAndReconcileIfRequired updates the resource pool and reconcile again and submit a new job if required
func (p *ipv4Provider) updatePoolAndReconcileIfRequired(resourcePool pool.Pool, job *worker.WarmPoolJob, didSucceed bool) {
	// Update the pool to add the created/failed resource to the warm pool and decrement the pending count
	shouldReconcile := resourcePool.UpdatePool(job, didSucceed)

	if shouldReconcile {
		job := resourcePool.ReconcilePool()
		if job.Operations != worker.OperationReconcileNotRequired {
			p.SubmitAsyncJob(job)
		}
	}
}

// putInstanceProviderAndPool stores the node's instance provider and pool to the cache
func (p *ipv4Provider) putInstanceProviderAndPool(nodeName string, resourcePool pool.Pool, manager eni.ENIManager) {
	p.lock.Lock()
	defer p.lock.Unlock()

	resource := ResourceProviderAndPool{
		eniManager:   manager,
		resourcePool: resourcePool,
	}

	p.instanceProviderAndPool[nodeName] = resource
}

// getInstanceProviderAndPool returns the node's instance provider and pool from the cache
func (p *ipv4Provider) getInstanceProviderAndPool(nodeName string) (ResourceProviderAndPool, bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	resource, found := p.instanceProviderAndPool[nodeName]
	return resource, found
}

// deleteInstanceProviderAndPool deletes the node's instance provider and pool from the cache
func (p *ipv4Provider) deleteInstanceProviderAndPool(nodeName string) {
	p.lock.Lock()
	defer p.lock.Unlock()

	delete(p.instanceProviderAndPool, nodeName)
}

// getCapacity returns the capacity based on the instance type and the instance os
func getCapacity(instanceType string, instanceOs string) int {
	// Assign only 1st ENIs non primary IP
	limits, found := vpc.Limits[instanceType]
	if !found {
		return 0
	}
	var capacity int
	if instanceOs == config.OSWindows {
		capacity = limits.IPv4PerInterface - 1
	} else {
		capacity = (limits.IPv4PerInterface - 1) * limits.Interface
	}

	return capacity
}

// difference returns the difference between the slice and the map in the argument
func difference(allIPs []string, usedIPSet map[string]struct{}) []string {
	var notUsed []string
	for _, ip := range allIPs {
		if _, found := usedIPSet[ip]; !found {
			notUsed = append(notUsed, ip)
		}
	}
	return notUsed
}

// GetPool returns the warm pool for the IPv4 resources
func (p *ipv4Provider) GetPool(nodeName string) (pool.Pool, bool) {
	providerAndPool, exists := p.getInstanceProviderAndPool(nodeName)
	if !exists {
		return nil, false
	}
	return providerAndPool.resourcePool, true
}

// IsInstanceSupported returns true for windows node as IP as extended resource is only supported by windows node now
func (p *ipv4Provider) IsInstanceSupported(instance ec2.EC2Instance) bool {
	if instance.Os() == config.OSWindows {
		return true
	}
	return false
}

func (p *ipv4Provider) Introspect() interface{} {
	p.lock.RLock()
	defer p.lock.RUnlock()

	response := make(map[string]pool.IntrospectResponse)
	for nodeName, resource := range p.instanceProviderAndPool {
		response[nodeName] = resource.resourcePool.Introspect()
	}
	return response
}

func (p *ipv4Provider) IntrospectNode(nodeName string) interface{} {
	p.lock.RLock()
	defer p.lock.RUnlock()

	resource, found := p.instanceProviderAndPool[nodeName]
	if !found {
		return struct{}{}
	}
	return resource.resourcePool.Introspect()
}
