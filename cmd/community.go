package cmd

import (
	"fmt"
	"strings"

	"github.com/kubeshark/kubeshark/config"
	"github.com/kubeshark/kubeshark/misc"
	"github.com/kubeshark/kubeshark/utils"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var communityCmd = &cobra.Command{
	Use:   "community",
	Short: fmt.Sprintf("Use %s Community Edition. (default)", misc.Software),
	RunE: func(cmd *cobra.Command, args []string) error {
		edition := "community"
		config.Config.Edition = edition
		if err := config.WriteConfig(&config.Config); err != nil {
			log.Error().Err(err).Msg("Failed writing config.")
			return nil
		}

		log.Info().Msgf("%s edition has been set to: %s", misc.Software, strings.Title(edition))

		log.Warn().
			Str("command", fmt.Sprintf("%s tap", misc.Program)).
			Msg(fmt.Sprintf(utils.Yellow, "Now you can run:"))

		return nil
	},
}

func init() {
	rootCmd.AddCommand(communityCmd)
}
