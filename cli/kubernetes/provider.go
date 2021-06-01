package kubernetes

import (
	_ "bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/up9inc/mizu/shared"

	"path/filepath"
	"regexp"

	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	applyconfapp "k8s.io/client-go/applyconfigurations/apps/v1"
	applyconfcore "k8s.io/client-go/applyconfigurations/core/v1"
	applyconfmeta "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	_ "k8s.io/client-go/plugin/pkg/client/auth/openstack"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	_ "k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/util/homedir"
)

type Provider struct {
	clientSet        *kubernetes.Clientset
	kubernetesConfig clientcmd.ClientConfig
	clientConfig     restclient.Config
	Namespace        string
}

const (
	serviceAccountName = "mizu-service-account"
	fieldManagerName   = "mizu-manager"
)

func NewProvider(kubeConfigPath string, overrideNamespace string) *Provider {
	kubernetesConfig := loadKubernetesConfiguration(kubeConfigPath)
	restClientConfig, err := kubernetesConfig.ClientConfig()
	if err != nil {
		panic(err.Error())
	}
	clientSet := getClientSet(restClientConfig)

	var namespace string
	if len(overrideNamespace) > 0 {
		namespace = overrideNamespace
	} else {
		configuredNamespace, _, err := kubernetesConfig.Namespace()
		if err != nil {
			panic(err)
		}
		namespace = configuredNamespace
	}

	return &Provider{
		clientSet:        clientSet,
		kubernetesConfig: kubernetesConfig,
		clientConfig:     *restClientConfig,
		Namespace:        namespace,
	}
}

func (provider *Provider) GetPodWatcher(ctx context.Context, namespace string) watch.Interface {
	watcher, err := provider.clientSet.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{Watch: true})
	if err != nil {
		panic(err.Error())
	}
	return watcher
}

func (provider *Provider) GetPods(ctx context.Context, namespace string) {
	pods, err := provider.clientSet.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}
	fmt.Printf("There are %d pods in Namespace %s\n", len(pods.Items), namespace)
}

func (provider *Provider) CreateMizuAggregatorPod(ctx context.Context, namespace string, podName string, podImage string, linkServiceAccount bool, mizuApiFilteringOptions *shared.TrafficFilteringOptions) (*core.Pod, error) {
	marshaledFilteringOptions, err := json.Marshal(mizuApiFilteringOptions)
	if err != nil {
		return nil, err
	}
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{"app": podName},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{
					Name:            podName,
					Image:           podImage,
					ImagePullPolicy: core.PullAlways,
					Command: []string {"./mizuagent", "--aggregator"},
					Env: []core.EnvVar{
						{
							Name: shared.HostModeEnvVar,
							Value: "1",
						},
						{
							Name: shared.MizuFilteringOptionsEnvVar,
							Value: string(marshaledFilteringOptions),
						},
					},
				},
			},
			DNSPolicy: "ClusterFirstWithHostNet",
			TerminationGracePeriodSeconds: new(int64),
			// Affinity: TODO: define node selector for all relevant nodes for this mizu instance
		},
	}
	//define the service account only when it exists to prevent pod crash
	if linkServiceAccount {
		pod.Spec.ServiceAccountName = serviceAccountName
	}
	return provider.clientSet.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
}

func (provider *Provider) CreateService(ctx context.Context, namespace string, serviceName string, appLabelValue string) (*core.Service, error) {
	service := core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceName,
			Namespace: namespace,
		},
		Spec: core.ServiceSpec{
			Ports: []core.ServicePort {{TargetPort: intstr.FromInt(8899), Port: 80}},
			Type: core.ServiceTypeClusterIP,
			Selector: map[string]string{"app": appLabelValue},
		},
	}
	return provider.clientSet.CoreV1().Services(namespace).Create(ctx, &service, metav1.CreateOptions{})
}

func (provider *Provider) DoesMizuRBACExist(ctx context.Context, namespace string) (bool, error){
	serviceAccount, err := provider.clientSet.CoreV1().ServiceAccounts(namespace).Get(ctx, serviceAccountName, metav1.GetOptions{})

	var statusError *k8serrors.StatusError
	if errors.As(err, &statusError) {
		// expected behavior when resource does not exist
		if statusError.ErrStatus.Reason == metav1.StatusReasonNotFound {
			return false, nil
		}
	}
	if err != nil {
		return false, err
	}
	return serviceAccount != nil, nil
}

func (provider *Provider) CreateMizuRBAC(ctx context.Context, namespace string ,version string) error {
	clusterRoleName := "mizu-cluster-role"

	serviceAccount := &core.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccountName,
			Namespace: namespace,
			Labels:    map[string]string{"mizu-cli-version": version},
		},
	}
	clusterRole := &rbac.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterRoleName,
			Labels: map[string]string{"mizu-cli-version": version},
		},
		Rules: []rbac.PolicyRule{
			{
				APIGroups: []string {"", "extensions", "apps"},
				Resources: []string {"pods", "services", "endpoints"},
				Verbs: []string {"list", "get", "watch"},
			},
		},
	}
	clusterRoleBinding := &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mizu-cluster-role-binding",
			Labels: map[string]string{"mizu-cli-version": version},
		},
		RoleRef: rbac.RoleRef{
			Name: clusterRoleName,
			Kind: "ClusterRole",
			APIGroup: "rbac.authorization.k8s.io",
		},
		Subjects: []rbac.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccountName,
				Namespace: namespace,
			},
		},
	}
	_, err := provider.clientSet.CoreV1().ServiceAccounts(namespace).Create(ctx, serviceAccount, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	_, err = provider.clientSet.RbacV1().ClusterRoles().Create(ctx, clusterRole, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	_, err = provider.clientSet.RbacV1().ClusterRoleBindings().Create(ctx, clusterRoleBinding, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (provider *Provider) RemovePod(ctx context.Context, namespace string, podName string) error {
	return provider.clientSet.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{})
}

func (provider *Provider) RemoveService(ctx context.Context, namespace string, serviceName string) error {
	return provider.clientSet.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
}

func (provider *Provider) RemoveDaemonSet(ctx context.Context, namespace string, daemonSetName string) error {
	return provider.clientSet.AppsV1().DaemonSets(namespace).Delete(ctx, daemonSetName, metav1.DeleteOptions{})
}

func (provider *Provider) ApplyMizuTapperDaemonSet(ctx context.Context, namespace string, daemonSetName string, podImage string, tapperPodName string, aggregatorPodIp string, nodeToTappedPodIPMap map[string][]string, linkServiceAccount bool) error {
	nodeToTappedPodIPMapJsonStr, err := json.Marshal(nodeToTappedPodIPMap)
	if err != nil {
		return err
	}

	privileged := true
	agentContainer := applyconfcore.Container()
	agentContainer.WithName(tapperPodName)
	agentContainer.WithImage(podImage)
	agentContainer.WithImagePullPolicy(core.PullAlways)
	agentContainer.WithSecurityContext(applyconfcore.SecurityContext().WithPrivileged(privileged))
	agentContainer.WithCommand("./mizuagent", "-i", "any", "--tap", "--hardump", "--aggregator-address", fmt.Sprintf("ws://%s/wsTapper", aggregatorPodIp))
	agentContainer.WithEnv(
		applyconfcore.EnvVar().WithName(shared.HostModeEnvVar).WithValue("1"),
		applyconfcore.EnvVar().WithName(shared.TappedAddressesPerNodeDictEnvVar).WithValue(string(nodeToTappedPodIPMapJsonStr)),
	)
	agentContainer.WithEnv(
		applyconfcore.EnvVar().WithName(shared.NodeNameEnvVar).WithValueFrom(
			applyconfcore.EnvVarSource().WithFieldRef(
				applyconfcore.ObjectFieldSelector().WithAPIVersion("v1").WithFieldPath("spec.nodeName"),
			),
		),
	)

	podSpec := applyconfcore.PodSpec().WithHostNetwork(true).WithDNSPolicy("ClusterFirstWithHostNet").WithTerminationGracePeriodSeconds(0)
	if linkServiceAccount {
		podSpec.WithServiceAccountName(serviceAccountName)
	}
	podSpec.WithContainers(agentContainer)


	podTemplate := applyconfcore.PodTemplateSpec()
	podTemplate.WithLabels(map[string]string{"app": tapperPodName})
	podTemplate.WithSpec(podSpec)

	labelSelector := applyconfmeta.LabelSelector()
	labelSelector.WithMatchLabels(map[string]string{"app": tapperPodName})

	daemonSet := applyconfapp.DaemonSet(daemonSetName, namespace)
	daemonSet.WithSpec(applyconfapp.DaemonSetSpec().WithSelector(labelSelector).WithTemplate(podTemplate))

	_, err = provider.clientSet.AppsV1().DaemonSets(namespace).Apply(ctx, daemonSet, metav1.ApplyOptions{FieldManager: fieldManagerName})
	return err
}

func (provider *Provider) GetAllPodsMatchingRegex(ctx context.Context, regex *regexp.Regexp) ([]core.Pod, error) {
	pods, err := provider.clientSet.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	matchingPods := make([]core.Pod, 0)
	for _, pod := range pods.Items {
		if regex.MatchString(pod.Name) {
			matchingPods = append(matchingPods, pod)
		}
	}
	return matchingPods, err
}

func getClientSet(config *restclient.Config) *kubernetes.Clientset {
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	return clientSet
}

func loadKubernetesConfiguration(kubeConfigPath string) clientcmd.ClientConfig {
	if kubeConfigPath == "" {
		home := homedir.HomeDir()
		kubeConfigPath = filepath.Join(home, ".kube", "config")
	}

	configPathList := filepath.SplitList(kubeConfigPath)
	configLoadingRules := &clientcmd.ClientConfigLoadingRules{}
	if len(configPathList) <= 1 {
		configLoadingRules.ExplicitPath = kubeConfigPath
	} else {
		configLoadingRules.Precedence = configPathList
	}
	contextName := ""
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		configLoadingRules,
		&clientcmd.ConfigOverrides{
			CurrentContext: contextName,
		},
	)
}
