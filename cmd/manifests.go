package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/creasty/defaults"
	"github.com/kubeshark/kubeshark/config"
	"github.com/kubeshark/kubeshark/docker"
	"github.com/kubeshark/kubeshark/kubernetes"
	"github.com/kubeshark/kubeshark/utils"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

const manifestSeperator = "---"

var manifestsCmd = &cobra.Command{
	Use:   "manifests",
	Short: "Generate Kubernetes manifests of Kubeshark",
	RunE: func(cmd *cobra.Command, args []string) error {
		runManifests()
		return nil
	},
}

func init() {
	rootCmd.AddCommand(manifestsCmd)

	defaultManifestsConfig := config.ManifestsConfig{}
	if err := defaults.Set(&defaultManifestsConfig); err != nil {
		log.Debug().Err(err).Send()
	}

	manifestsCmd.Flags().Bool("dump", defaultManifestsConfig.Dump, "Enable the debug mode")
}

func runManifests() {
	kubernetesProvider, err := getKubernetesProviderForCli(true)
	if err != nil {
		log.Error().Err(err).Send()
		return
	}

	namespace := kubernetesProvider.BuildNamespace(config.Config.Tap.SelfNamespace)

	serviceAccount := kubernetesProvider.BuildServiceAccount()

	clusterRole := kubernetesProvider.BuildClusterRole()

	clusterRoleBinding := kubernetesProvider.BuildClusterRoleBinding()

	hubPod, err := kubernetesProvider.BuildHubPod(&kubernetes.PodOptions{
		Namespace:          config.Config.Tap.SelfNamespace,
		PodName:            kubernetes.HubPodName,
		PodImage:           docker.GetHubImage(),
		ServiceAccountName: kubernetes.ServiceAccountName,
		Resources:          config.Config.Tap.Resources.Hub,
		ImagePullPolicy:    config.Config.ImagePullPolicy(),
		ImagePullSecrets:   config.Config.ImagePullSecrets(),
		Debug:              config.Config.Tap.Debug,
	})
	if err != nil {
		log.Error().Err(err).Send()
		return
	}

	hubService := kubernetesProvider.BuildHubService(config.Config.Tap.SelfNamespace)

	frontPod, err := kubernetesProvider.BuildFrontPod(&kubernetes.PodOptions{
		Namespace:          config.Config.Tap.SelfNamespace,
		PodName:            kubernetes.FrontPodName,
		PodImage:           docker.GetHubImage(),
		ServiceAccountName: kubernetes.ServiceAccountName,
		Resources:          config.Config.Tap.Resources.Hub,
		ImagePullPolicy:    config.Config.ImagePullPolicy(),
		ImagePullSecrets:   config.Config.ImagePullSecrets(),
		Debug:              config.Config.Tap.Debug,
	}, config.Config.Tap.Proxy.Host, fmt.Sprintf("%d", config.Config.Tap.Proxy.Hub.SrcPort))
	if err != nil {
		log.Error().Err(err).Send()
		return
	}

	frontService := kubernetesProvider.BuildFrontService(config.Config.Tap.SelfNamespace)

	workerDaemonSet, err := kubernetesProvider.BuildWorkerDaemonSet(
		kubernetes.WorkerDaemonSetName,
		kubernetes.WorkerPodName,
		kubernetes.ServiceAccountName,
		config.Config.Tap.Resources.Worker,
		config.Config.ImagePullPolicy(),
		config.Config.ImagePullSecrets(),
		config.Config.Tap.ServiceMesh,
		config.Config.Tap.Tls,
		config.Config.Tap.Debug,
	)
	if err != nil {
		log.Error().Err(err).Send()
		return
	}

	if config.Config.Manifests.Dump {
		err = dumpManifests(map[string]interface{}{
			"00-namespace.yaml":            namespace,
			"01-service-account.yaml":      serviceAccount,
			"02-cluster-role.yaml":         clusterRole,
			"03-cluster-role-binding.yaml": clusterRoleBinding,
			"04-hub-pod.yaml":              hubPod,
			"05-hub-service.yaml":          hubService,
			"06-front-pod.yaml":            frontPod,
			"07-front-service.yaml":        frontService,
			"08-worker-daemon-set.yaml":    workerDaemonSet,
		})
	} else {
		err = printManifests([]interface{}{
			namespace,
			serviceAccount,
			clusterRole,
			clusterRoleBinding,
			hubPod,
			hubService,
			frontPod,
			frontService,
			workerDaemonSet,
		})
	}
	if err != nil {
		log.Error().Err(err).Send()
		return
	}
}

func dumpManifests(objects map[string]interface{}) error {
	folder := filepath.Join(".", "manifests")
	err := os.MkdirAll(folder, os.ModePerm)
	if err != nil {
		return err
	}

	// Sort by filenames
	filenames := make([]string, 0)
	for filename := range objects {
		filenames = append(filenames, filename)
	}
	sort.Strings(filenames)

	for _, filename := range filenames {
		manifest, err := utils.PrettyYamlOmitEmpty(objects[filename])
		if err != nil {
			return err
		}

		path := filepath.Join(folder, filename)
		err = os.WriteFile(path, []byte(manifest), 0644)
		if err != nil {
			return err
		}
		log.Info().Msgf("Manifest generated: %s", path)
	}

	return nil
}

func printManifests(objects []interface{}) error {
	for _, object := range objects {
		manifest, err := utils.PrettyYamlOmitEmpty(object)
		if err != nil {
			return err
		}
		fmt.Println(manifestSeperator)
		fmt.Println(manifest)
	}

	return nil
}
