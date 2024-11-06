// SPDX-FileCopyrightText: 2022 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gardener/aws-custom-route-controller/pkg/controller"
	"github.com/gardener/aws-custom-route-controller/pkg/updater"
	"github.com/gardener/aws-custom-route-controller/pkg/util"
	"github.com/gardener/aws-custom-route-controller/pkg/util/logger"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Version is injected by build
var Version string

const (
	// componentName is the component name
	componentName = "aws-custom-route-controller"
	// leaderElectionId is the name of the lease resource
	leaderElectionId = "aws-custom-route-controller-leader-election"
)

var (
	clusterName             = pflag.String("cluster-name", "", "cluster name used for AWS tags")
	controlKubeconfig       = pflag.String("control-kubeconfig", updater.InClusterConfig, fmt.Sprintf("path of control plane kubeconfig or '%s' for in-cluster config", updater.InClusterConfig))
	healthProbePort         = pflag.Int("health-probe-port", 8081, "port for health probes")
	maxDelay                = pflag.Duration("max-delay-on-failure", 5*time.Minute, "maximum delay if communication with AWS fails")
	metricsPort             = pflag.Int("metrics-port", 8080, "port for metrics")
	namespace               = pflag.String("namespace", "", "namespace of secret containing the AWS credentials on control plane")
	podNetworkCidr          = pflag.String("pod-network-cidr", "", "CIDR for pod network")
	region                  = pflag.String("region", "", "AWS region")
	secretName              = pflag.String("secret-name", "cloudprovider", "name of secret containing the AWS credentials on control plane")
	syncPeriod              = pflag.Duration("sync-period", 1*time.Hour, "period for syncing routes")
	targetKubeconfig        = pflag.String("target-kubeconfig", "", "path of target kubeconfig")
	tickPeriod              = pflag.Duration("tick-period", 5*time.Second, "tick period for checking for updates")
	leaderElection          = pflag.Bool("leader-election", false, "enable leader election")
	leaderElectionNamespace = pflag.String("leader-election-namespace", "kube-system", "namespace for the lease resource")
	logLevel                = pflag.String("log-level", logger.InfoLevel, "LogLevel is the level/severity for the logs. Must be one of [info,debug,error].")
	logFormat               = pflag.String("log-format", logger.FormatJSON, "output format for the logs. Must be one of [text,json].")
)

func main() {

	logf.SetLogger(logger.MustNewZapLogger(*logLevel, *logFormat))

	var log = logf.Log.WithName(componentName)
	klog.SetLogger(log)
	log.Info("version", "version", Version)

	pflag.Parse()
	checkRequiredFlag(log, "namespace", *namespace)
	checkRequiredFlag(log, "secret-name", *secretName)
	checkRequiredFlag(log, "region", *region)
	checkRequiredFlag(log, "cluster-name", *clusterName)
	checkRequiredFlag(log, "pod-network-cidr", *podNetworkCidr)
	checkRequiredFlag(log, "target-kubeconfig", *targetKubeconfig)

	targetConfig, err := clientcmd.BuildConfigFromFlags("", *targetKubeconfig)
	if err != nil {
		log.Error(err, "could not use target kubeconfig", "target-kubeconfig", *targetKubeconfig)
		os.Exit(1)
	}
	options := manager.Options{
		LeaderElection:             *leaderElection,
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		LeaderElectionID:           leaderElectionId,
		LeaderElectionNamespace:    *leaderElectionNamespace,
		Metrics: server.Options{
			BindAddress: fmt.Sprintf(":%d", *metricsPort),
		},
		HealthProbeBindAddress: fmt.Sprintf(":%d", *healthProbePort),
	}
	mgr, err := manager.New(targetConfig, options)
	if err != nil {
		log.Error(err, "could not create manager")
		os.Exit(1)
	}

	reconciler := controller.NewNodeReconciler(mgr.GetClient(), log, mgr.Elected(), mgr.GetEventRecorderFor(componentName))
	err = builder.
		ControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Complete(reconciler)
	if err != nil {
		log.Error(err, "could not create controller")
		os.Exit(1)
	}
	err = mgr.AddReadyzCheck("node reconciler", reconciler.ReadyChecker)
	if err != nil {
		log.Error(err, "could not add ready checker")
		os.Exit(1)
	}
	err = mgr.AddHealthzCheck("node reconciler", reconciler.HealthzChecker)
	if err != nil {
		log.Error(err, "could not add healthz checker")
		os.Exit(1)
	}

	credentials, err := updater.LoadCredentials(*controlKubeconfig, *namespace, *secretName)
	if err != nil {
		log.Error(err, "could not load AWS credentials", "namespace", *namespace, "secretName", *secretName)
		os.Exit(1)
	}
	ec2Routes, err := updater.NewAWSEC2Routes(credentials, *region)
	if err != nil {
		log.Error(err, "could not create AWS EC2 interface")
		os.Exit(1)
	}
	podCIDR, err := util.GetIPv4CIDR(strings.Split(*podNetworkCidr, ","))
	if err != nil {
		log.Error(err, "could not parse IPv4 address from pod-network-cidr")
		os.Exit(1)
	}

	customRoutes, err := updater.NewCustomRoutes(log.WithName("updater"), ec2Routes, *clusterName, podCIDR)
	if err != nil {
		log.Error(err, "could not create AWS custom routes updater")
		os.Exit(1)
	}

	ctx := signals.SetupSignalHandler()
	reconciler.StartUpdater(ctx, customRoutes.Update, *tickPeriod, *syncPeriod, *maxDelay)
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "could not start manager")
		os.Exit(1)
	}
}

func checkRequiredFlag(log logr.Logger, name, value string) {
	if value == "" {
		log.Info(fmt.Sprintf("'--%s' is required", name))
		pflag.Usage()
		os.Exit(1)
	}
}
