package operator

import (
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/staticpod"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	configv1 "github.com/openshift/api/config/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/cluster-kube-controller-manager-operator/pkg/apis/kubecontrollermanager/v1alpha1"
	operatorconfigclient "github.com/openshift/cluster-kube-controller-manager-operator/pkg/generated/clientset/versioned"
	operatorclientinformers "github.com/openshift/cluster-kube-controller-manager-operator/pkg/generated/informers/externalversions"
	"github.com/openshift/cluster-kube-controller-manager-operator/pkg/operator/configobservation/configobservercontroller"
	"github.com/openshift/cluster-kube-controller-manager-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-kube-controller-manager-operator/pkg/operator/resourcesynccontroller"
	"github.com/openshift/cluster-kube-controller-manager-operator/pkg/operator/targetconfigcontroller"
	"github.com/openshift/cluster-kube-controller-manager-operator/pkg/operator/v311_00_assets"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/revision"
)

func RunOperator(ctx *controllercmd.ControllerContext) error {

	kubeClient, err := kubernetes.NewForConfig(ctx.KubeConfig)
	if err != nil {
		return err
	}
	operatorConfigClient, err := operatorconfigclient.NewForConfig(ctx.KubeConfig)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(ctx.KubeConfig)
	if err != nil {
		return err
	}
	configClient, err := configv1client.NewForConfig(ctx.KubeConfig)
	if err != nil {
		return err
	}

	operatorConfigInformers := operatorclientinformers.NewSharedInformerFactory(operatorConfigClient, 10*time.Minute)
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient,
		"",
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.GlobalMachineSpecifiedConfigNamespace,
		operatorclient.OperatorNamespace,
		operatorclient.TargetNamespace,
		"kube-system",
	)
	operatorClient := &operatorclient.OperatorClient{
		Informers: operatorConfigInformers,
		Client:    operatorConfigClient.KubecontrollermanagerV1alpha1(),
	}

	v1helpers.EnsureOperatorConfigExists(
		dynamicClient,
		v311_00_assets.MustAsset("v3.11.0/kube-controller-manager/operator-config.yaml"),
		schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: "v1alpha1", Resource: "kubecontrollermanageroperatorconfigs"},
	)

	resourceSyncController, err := resourcesynccontroller.NewResourceSyncController(
		operatorClient,
		kubeInformersForNamespaces,
		kubeClient,
		ctx.EventRecorder,
	)
	if err != nil {
		return err
	}
	configObserver := configobservercontroller.NewConfigObserver(
		operatorClient,
		operatorConfigInformers,
		kubeInformersForNamespaces.InformersFor("kube-system"),
		resourceSyncController,
		ctx.EventRecorder,
	)
	targetConfigController := targetconfigcontroller.NewTargetConfigController(
		os.Getenv("IMAGE"),
		kubeInformersForNamespaces,
		operatorConfigInformers.Kubecontrollermanager().V1alpha1().KubeControllerManagerOperatorConfigs(),
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace),
		operatorConfigClient.KubecontrollermanagerV1alpha1(),
		operatorClient,
		kubeClient,
		ctx.EventRecorder,
	)

	staticPodControllers := staticpod.NewControllers(
		operatorclient.TargetNamespace,
		"openshift-kube-controller-manager",
		"kube-controller-manager-pod",
		[]string{"cluster-kube-controller-manager-operator", "installer"},
		[]string{"cluster-kube-controller-manager-operator", "prune"},
		deploymentConfigMaps,
		deploymentSecrets,
		operatorClient,
		v1helpers.CachedConfigMapGetter(kubeClient, kubeInformersForNamespaces),
		v1helpers.CachedSecretGetter(kubeClient, kubeInformersForNamespaces),
		kubeClient.CoreV1(),
		kubeClient,
		dynamicClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace),
		kubeInformersForNamespaces.InformersFor(""),
		ctx.EventRecorder,
	)

	clusterOperatorStatus := status.NewClusterOperatorStatusController(
		"kube-controller-manager",
		[]configv1.ObjectReference{
			{Group: "kubecontrollermanager.operator.openshift.io", Resource: "kubecontrollermanageroperatorconfigs", Name: "cluster"},
			{Resource: "namespaces", Name: "openshift-config"},
			{Resource: "namespaces", Name: "openshift-config-managed"},
			{Resource: "namespaces", Name: operatorclient.TargetNamespace},
			{Resource: "namespaces", Name: "openshift-kube-controller-manager-operator"},
		},
		configClient.ConfigV1(),
		operatorClient,
		status.NewVersionGetter(),
		ctx.EventRecorder,
	)

	operatorConfigInformers.Start(ctx.Context.Done())
	kubeInformersForNamespaces.Start(ctx.Context.Done())

	go staticPodControllers.Run(ctx.Context.Done())
	go targetConfigController.Run(1, ctx.Context.Done())
	go configObserver.Run(1, ctx.Context.Done())
	go clusterOperatorStatus.Run(1, ctx.Context.Done())
	go resourceSyncController.Run(1, ctx.Context.Done())

	<-ctx.Context.Done()
	return fmt.Errorf("stopped")
}

// deploymentConfigMaps is a list of configmaps that are directly copied for the current values.  A different actor/controller modifies these.
// the first element should be the configmap that contains the static pod manifest
var deploymentConfigMaps = []revision.RevisionResource{
	{Name: "kube-controller-manager-pod"},

	{Name: "config"},
	{Name: "serviceaccount-ca"},
}

// deploymentSecrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
var deploymentSecrets = []revision.RevisionResource{
	{Name: "cluster-signing-ca"},
	{Name: "controller-manager-kubeconfig"},
	{Name: "service-account-private-key"},
	{Name: "serving-cert"},
}
