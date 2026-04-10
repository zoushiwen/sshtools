package cmd

import (
	"github.com/spf13/cobra"

	"sshtools/internal/ui"
)

func Execute() error {
	return newRootCmd().Execute()
}

func newRootCmd() *cobra.Command {
	var configPath string
	var showVersion bool

	rootCmd := &cobra.Command{
		Use:          "ssh-tool",
		Short:        "JumpServer 风格的 SSH 管理工具",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				cmd.Println("sshtools " + Version)
				return nil
			}

			app, err := ui.NewApp(configPath)
			if err != nil {
				return err
			}

			return app.Run()
		},
	}

	rootCmd.Flags().StringVar(&configPath, "config", "", "指定配置文件路径")
	rootCmd.Flags().BoolVarP(&showVersion, "version", "v", false, "显示版本信息")

	return rootCmd
}
