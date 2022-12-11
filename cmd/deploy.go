package cmd

import (
	"errors"

	"github.com/creasty/defaults"
	"github.com/kubeshark/kubeshark/config"
	"github.com/kubeshark/kubeshark/config/configStructs"
	"github.com/kubeshark/kubeshark/errormessage"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var deployCmd = &cobra.Command{
	Use:   "deploy [POD REGEX]",
	Short: "Deploy Kubeshark into your K8s cluster.",
	Long:  `Deploy Kubeshark into your K8s cluster to gain visibility.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		deploy()
		return nil
	},
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			config.Config.Deploy.PodRegexStr = args[0]
		} else if len(args) > 1 {
			return errors.New("unexpected number of arguments")
		}

		if err := config.Config.Deploy.Validate(); err != nil {
			return errormessage.FormatError(err)
		}

		log.Info().
			Str("limit", config.Config.Deploy.HumanMaxEntriesDBSize).
			Msg("Kubeshark will store the traffic up to a limit. Oldest entries will be removed once the limit is reached.")

		return nil
	},
}

func init() {
	rootCmd.AddCommand(deployCmd)

	defaultDeployConfig := configStructs.DeployConfig{}
	if err := defaults.Set(&defaultDeployConfig); err != nil {
		log.Debug().Err(err).Send()
	}

	deployCmd.Flags().StringP(configStructs.TagLabel, "t", defaultDeployConfig.Tag, "The tag of the Docker images that are going to be deployed.")
	deployCmd.Flags().Uint16P(configStructs.ProxyPortLabel, "p", defaultDeployConfig.ProxyPort, "Provide a custom port for the web interface webserver.")
	deployCmd.Flags().StringSliceP(configStructs.NamespacesLabel, "n", defaultDeployConfig.Namespaces, "Namespaces selector.")
	deployCmd.Flags().BoolP(configStructs.AllNamespacesLabel, "A", defaultDeployConfig.AllNamespaces, "Deploy to all namespaces.")
	deployCmd.Flags().Bool(configStructs.EnableRedactionLabel, defaultDeployConfig.EnableRedaction, "Enables redaction of potentially sensitive request/response headers and body values.")
	deployCmd.Flags().String(configStructs.HumanMaxEntriesDBSizeLabel, defaultDeployConfig.HumanMaxEntriesDBSize, "Override the default max entries db size.")
	deployCmd.Flags().String(configStructs.InsertionFilterName, defaultDeployConfig.InsertionFilter, "Set the insertion filter. Accepts string or a file path.")
	deployCmd.Flags().Bool(configStructs.DryRunLabel, defaultDeployConfig.DryRun, "Preview of all pods matching the regex, without deploying workers on them.")
	deployCmd.Flags().Bool(configStructs.ServiceMeshName, defaultDeployConfig.ServiceMesh, "Record decrypted traffic if the cluster is configured with a service mesh and with mtls.")
	deployCmd.Flags().Bool(configStructs.TlsName, defaultDeployConfig.Tls, "Record tls traffic.")
	deployCmd.Flags().Bool(configStructs.ProfilerName, defaultDeployConfig.Profiler, "Run pprof server.")
	deployCmd.Flags().Int(configStructs.MaxLiveStreamsName, defaultDeployConfig.MaxLiveStreams, "Maximum live tcp streams to handle concurrently.")
}
