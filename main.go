/*


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

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	_ "net/http"
	_ "net/http/pprof"
	"os"
	"time"

	zapRaw "go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	crdv1alpha1 "github.com/aws/amazon-vpc-cni-k8s/pkg/apis/crd/v1alpha1"
	vpcresourcesv1beta1 "github.com/aws/amazon-vpc-resource-controller-k8s/apis/vpcresources/v1beta1"
	corecontroller "github.com/aws/amazon-vpc-resource-controller-k8s/controllers/core"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/aws/ec2/api"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/config"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/handler"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/k8s"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/node"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/provider"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/provider/branch"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/provider/ip"
	webhookutils "github.com/aws/amazon-vpc-resource-controller-k8s/pkg/utils"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/version"
	"github.com/aws/amazon-vpc-resource-controller-k8s/pkg/worker"
	webhookcore "github.com/aws/amazon-vpc-resource-controller-k8s/webhook/core"
	// +kubebuilder:scaffold:imports
)

var (
	scheme     = runtime.NewScheme()
	setupLog   = ctrl.Log.WithName("setup")
	syncPeriod = time.Minute * 30
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = corev1.AddToScheme(scheme)
	_ = vpcresourcesv1beta1.AddToScheme(scheme)
	_ = crdv1alpha1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

// +kubebuilder:rbac:groups=crd.k8s.amazonaws.com,resources=eniconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=vpcresources.k8s.aws,resources=securitygrouppolicies,verbs=get;list;watch

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var enableDevLogging bool
	var roleARN string
	var enableProfiling bool
	var logLevel string
	var clusterName string

	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&roleARN, "role-arn", "", "Role ARN that will be assumed to make EC2 API calls "+
		"to perform operations on the user's VPC. This parameter is not required if running the controller on your worker node.")
	flag.StringVar(&logLevel, "log-level", "info", "Set the controller log level - info(default), debug")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&enableDevLogging, "enable-dev-logging", false,
		"Enable developer mode logging for the controller."+
			"With dev mode logging, you will get Debug logs and more structured logging with extra details")
	flag.BoolVar(&enableProfiling, "enable-profiling", false, "Enable runtime profiling for debugging"+
		"purposes.")
	flag.StringVar(&clusterName, "cluster-name", "", "The name of the k8s cluster")

	flag.Parse()

	// Dev mode logging disabled by default, to enable set the enableDevLogging argument
	logLvl := zapRaw.NewAtomicLevelAt(0)
	if logLevel == "debug" {
		logLvl = zapRaw.NewAtomicLevelAt(-1)
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(enableDevLogging), zap.Level(&logLvl)))

	// Variables injected with ldflags on building the binary
	setupLog.Info("version",
		"GitVersion", version.GitVersion,
		"GitCommit", version.GitCommit,
		"BuildDate", version.BuildDate,
	)

	if clusterName == "" {
		setupLog.Error(fmt.Errorf("cluster-name is a required parameter"), "unable to start the controller")
		os.Exit(1)
	}

	// Profiler disabled by default, to enable set the enableProfiling argument
	if enableProfiling {
		// To use the profiler - https://golang.org/pkg/net/http/pprof/
		go func() {
			setupLog.Info("starting profiler",
				"error", http.ListenAndServe("localhost:6060", nil))
		}()
	}

	kubeConfig := ctrl.GetConfigOrDie()
	// Set the API Server QPS and Burst
	kubeConfig.QPS = config.DefaultAPIServerQPS
	kubeConfig.Burst = config.DefaultAPIServerBurst
	mgr, err := ctrl.NewManager(kubeConfig, ctrl.Options{
		SyncPeriod:              &syncPeriod,
		Scheme:                  scheme,
		MetricsBindAddress:      metricsAddr,
		Port:                    9443,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        config.LeaderElectionKey,
		LeaderElectionNamespace: config.LeaderElectionNamespace,
		HealthProbeBindAddress:  ":61779", // the liveness endpoint is default to "/healthz"
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// With kube-builder, we have to manually specify the fields on which the objects must be indexed, in order to
	// list objects using the k8s cache with field selectors
	err = mgr.GetFieldIndexer().IndexField(&corev1.Pod{}, "spec.nodeName", func(object runtime.Object) []string {
		pod := object.(*corev1.Pod)
		return []string{pod.Spec.NodeName}
	})
	if err != nil {
		setupLog.Error(err, "failed to add index field", "spec.nodeName")
		os.Exit(1)
	}

	clientSet, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		setupLog.Error(err, "failed to create client set")
		os.Exit(1)
	}

	// Add liveness probe
	err = mgr.AddHealthzCheck("health-ping", healthz.Ping)
	setupLog.Info("adding health check for controller")
	if err != nil {
		setupLog.Error(err, "unable add a health check")
		os.Exit(1)
	}

	// creating a cache helper to handle security groups.
	cacheHelper := webhookutils.NewK8sCacheHelper(
		mgr.GetClient(),
		ctrl.Log.WithName("cache helper"))

	// Get the resource providers and handlers
	resourceHandlers, nodeManager := setUpResources(mgr, clientSet, cacheHelper, roleARN, clusterName)

	if err = (&corecontroller.PodReconciler{
		Client:         mgr.GetClient(),
		Log:            ctrl.Log.WithName("controllers").WithName("Pod"),
		DeletePodQueue: make(map[string]*corev1.Pod),
		Scheme:         mgr.GetScheme(),
		Manager:        nodeManager,
		Handlers:       resourceHandlers,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Pod")
		os.Exit(1)
	}
	if err = (&corecontroller.NodeReconciler{
		Client:  mgr.GetClient(),
		Log:     ctrl.Log.WithName("controllers").WithName("Node"),
		Scheme:  mgr.GetScheme(),
		Manager: nodeManager,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Node")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder
	setupLog.Info("setting up webhook server")
	webhookServer := mgr.GetWebhookServer()

	setupLog.Info("registering webhooks to the webhook server")
	webhookServer.Register("/mutate-v1-pod", &webhook.Admission{Handler: &webhookcore.PodResourceInjector{
		Client:      mgr.GetClient(),
		CacheHelper: cacheHelper,
		Log:         ctrl.Log.WithName("webhook").WithName("Pod Mutating"),
	}})

	// Validating webhook for pod.
	webhookServer.Register("/validate-v1-pod", &webhook.Admission{Handler: &webhookcore.AnnotationValidator{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("webhook").WithName("Annotation Validator"),
	}})

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// setUpResources sets up all resource providers and the node manager
func setUpResources(manager manager.Manager, clientSet *kubernetes.Clientset,
	cacheHelper webhookutils.K8sCacheHelper, roleARN string, clusterName string) ([]handler.Handler, node.Manager) {

	var resourceProviders []provider.ResourceProvider

	ec2Wrapper, err := api.NewEC2Wrapper(roleARN, setupLog)
	if err != nil {
		setupLog.Error(err, "unable to create ec2 wrapper")
	}
	eniCleaner := api.NewENICleaner(ec2Wrapper, clusterName, context.Background(), ctrl.Log.WithName("eni cleaner"))
	manager.Add(eniCleaner)

	ec2APIHelper := api.NewEC2APIHelper(ec2Wrapper, clusterName)
	k8sWrapper := k8s.NewK8sWrapper(manager.GetClient(), clientSet.CoreV1())

	// Load the default resource config
	resourceConfig := config.LoadResourceConfig()

	// Set up on demand handlers
	onDemandProviders := getOnDemandResourceProviders(resourceConfig, k8sWrapper, ec2APIHelper, &resourceProviders, cacheHelper)
	onDemandHandler := handler.NewOnDemandHandler(ctrl.Log.WithName("on demand handler"), onDemandProviders)

	// Set up warm resource handlers
	warmResourceProviders := getWarmResourceProviders(resourceConfig, k8sWrapper, ec2APIHelper, &resourceProviders)
	warmResourceHandler := handler.NewWarmResourceHandler(ctrl.Log.WithName("warm resource handler"),
		k8sWrapper, warmResourceProviders)

	// Set up the node manager
	nodeManager := node.NewNodeManager(ctrl.Log.WithName("node manager"), resourceProviders, ec2APIHelper, k8sWrapper)

	return []handler.Handler{onDemandHandler, warmResourceHandler}, nodeManager
}

// getOnDemandResourceProviders returns all the providers for resource type on demand
func getOnDemandResourceProviders(resourceConfig map[string]config.ResourceConfig, k8sWrapper k8s.K8sWrapper,
	ec2APIHelper api.EC2APIHelper, providers *[]provider.ResourceProvider,
	cacheHelper webhookutils.K8sCacheHelper) map[string]provider.ResourceProvider {

	// Load Branch ENI Config
	branchConfig := resourceConfig[config.ResourceNamePodENI]

	// Create the branch provider and worker pool
	branchWorker := worker.NewDefaultWorkerPool(branchConfig.Name, branchConfig.WorkerCount,
		config.WorkQueueDefaultMaxRetries, ctrl.Log.WithName("branch eni worker"), context.Background())
	branchProvider := branch.NewBranchENIProvider(ctrl.Log.WithName("branch eni provider"),
		k8sWrapper, ec2APIHelper, branchWorker, cacheHelper)

	// Start the branch worker to accept new jobs on the give function
	err := branchWorker.StartWorkerPool(branchProvider.ProcessAsyncJob)
	if err != nil {
		setupLog.Error(err, "unable to start the branch ENI worker")
		os.Exit(1)
	}

	// Add provider to the list of providers
	*providers = append(*providers, branchProvider)

	return map[string]provider.ResourceProvider{branchConfig.Name: branchProvider}
}

// getWarmResourceProviders returns all the warm resource providers
func getWarmResourceProviders(resourceConfig map[string]config.ResourceConfig, k8sWrapper k8s.K8sWrapper,
	ec2APIHelper api.EC2APIHelper, providers *[]provider.ResourceProvider) map[string]provider.ResourceProvider {

	ipV4Config := resourceConfig[config.ResourceNameIPAddress]

	ipv4Worker := worker.NewDefaultWorkerPool(ipV4Config.Name, ipV4Config.WorkerCount,
		config.WorkQueueDefaultMaxRetries, ctrl.Log.WithName("secondary ipv4 worker"), context.Background())
	ipv4Provider := ip.NewIPv4Provider(ctrl.Log.WithName("secondary ipv4 provider"),
		ipV4Config.WarmPoolConfig, ec2APIHelper, ipv4Worker, k8sWrapper)

	err := ipv4Worker.StartWorkerPool(ipv4Provider.ProcessAsyncJob)
	if err != nil {
		setupLog.Error(err, "unable to start the ipv4 worker")
		os.Exit(1)
	}

	*providers = append(*providers, ipv4Provider)

	return map[string]provider.ResourceProvider{ipV4Config.Name: ipv4Provider}

}
