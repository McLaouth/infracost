package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/infracost/infracost/internal/config"
	"github.com/infracost/infracost/internal/output"
	"github.com/infracost/infracost/internal/prices"
	"github.com/infracost/infracost/internal/providers"
	"github.com/infracost/infracost/internal/schema"
	"github.com/infracost/infracost/internal/ui"
	"github.com/infracost/infracost/internal/usage"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func addDeprecatedRunFlags(cmd *cobra.Command) {
	cmd.Flags().String("tfjson", "", "Path to Terraform plan JSON file")
	_ = cmd.Flags().MarkHidden("tfjson")

	cmd.Flags().String("tfplan", "", "Path to Terraform plan file relative to 'terraform-dir'")
	_ = cmd.Flags().MarkHidden("tfplan")

	cmd.Flags().String("tfflags", "", "Flags to pass to the 'terraform plan' command")
	_ = cmd.Flags().MarkHidden("tfflags")

	cmd.Flags().String("tfdir", "", "Path to the Terraform code directory. Defaults to current working directory")
	_ = cmd.Flags().MarkHidden("tfdir")

	cmd.Flags().Bool("use-tfstate", false, "Use Terraform state instead of generating a plan")
	_ = cmd.Flags().MarkHidden("use-tfstate")

	cmd.Flags().StringP("output", "o", "table", "Output format: json, table, html")
	_ = cmd.Flags().MarkHidden("output")

	cmd.Flags().String("pricing-api-endpoint", "", "Specify an alternate Cloud Pricing API URL")
	_ = cmd.Flags().MarkHidden("pricing-api-endpoint")

	cmd.Flags().String("terraform-json-file", "", "Path to Terraform plan JSON file")
	_ = cmd.Flags().MarkHidden("terraform-json-file")

	cmd.Flags().String("terraform-plan-file", "", "Path to Terraform plan file relative to 'terraform-dir'")
	_ = cmd.Flags().MarkHidden("terraform-plan-file")

	cmd.Flags().String("terraform-dir", "", "Path to the Terraform code directory. Defaults to current working directory")
	_ = cmd.Flags().MarkHidden("terraform-dir")
}

func addRunInputFlags(cmd *cobra.Command) {
	cmd.Flags().String("path", "", "Path to the code directory or file")
	cmd.Flags().String("config-file", "", "Path to the Infracost config file. Cannot be used with other flags")
	cmd.Flags().String("usage-file", "", "Path to Infracost usage file that specifies values for usage-based resources")
	cmd.Flags().String("terraform-plan-flags", "", "Flags to pass to the 'terraform plan' command")
}

func addRunOutputFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("show-skipped", false, "Show unsupported resources, some of which might be free. Ignored for JSON outputs")
}

func runMain(cfg *config.Config) error {
	projects := make([]*schema.Project, 0)

	for _, projectCfg := range cfg.Projects.Terraform {
		cfg.Environment.SetTerraformEnvironment(projectCfg)

		provider := providers.Detect(cfg, projectCfg)
		if provider == nil {
			return errors.New("Could not detect path type")
		}

		m := fmt.Sprintf("Detected %s at %s", provider.Type(), projectCfg.Path)
		if cfg.IsLogging() {
			log.Info(m)
		} else {
			fmt.Fprintln(os.Stderr, m)
		}

		u, err := usage.LoadFromFile(projectCfg.UsageFile)
		if err != nil {
			return err
		}
		if len(u) > 0 {
			cfg.Environment.HasUsageFile = true
		}

		project, err := provider.LoadResources(u)
		if err != nil {
			return err
		}

		projects = append(projects, project)
	}

	spinnerOpts := ui.SpinnerOptions{
		EnableLogging: cfg.IsLogging(),
		NoColor:       cfg.NoColor,
	}
	spinner := ui.NewSpinner("Calculating cost estimate", spinnerOpts)

	for _, project := range projects {
		if err := prices.PopulatePrices(cfg, project); err != nil {
			spinner.Fail()
			fmt.Fprintln(os.Stderr, "")

			if e := unwrapped(err); errors.Is(e, prices.ErrInvalidAPIKey) {
				return errors.New(fmt.Sprintf("%v\n%s %s %s %s %s\n%s",
					e.Error(),
					"Please check your",
					ui.PrimaryString(config.CredentialsFilePath()),
					"file or",
					ui.PrimaryString("INFRACOST_API_KEY"),
					"environment variable.",
					"If you continue having issues please email hello@infracost.io",
				))
			}

			if e, ok := err.(*prices.PricingAPIError); ok {
				return errors.New(fmt.Sprintf("%v\n%s", e.Error(), "We have been notified of this issue."))
			}

			return err
		}

		schema.CalculateCosts(project)
		project.CalculateDiff()
	}

	spinner.Success()

	r := output.ToOutputFormat(projects)

	for _, outputCfg := range cfg.Outputs {
		cfg.Environment.SetOutputEnvironment(outputCfg)

		opts := output.Options{
			ShowSkipped: outputCfg.ShowSkipped,
			NoColor:     cfg.NoColor,
		}

		var (
			b   []byte
			out string
			err error
		)

		switch strings.ToLower(outputCfg.Format) {
		case "json":
			b, err = output.ToJSON(r, opts)
			out = string(b)
		case "html":
			b, err = output.ToHTML(r, opts)
			out = string(b)
		case "diff":
			b, err = output.ToDiff(r, opts)
			out = fmt.Sprintf("\n%s", string(b))
		case "table_deprecated":
			b, err = output.ToTableDeprecated(r, opts)
			out = fmt.Sprintf("\n%s", string(b))
		default:
			b, err = output.ToTable(r, opts)
			out = fmt.Sprintf("\n%s", string(b))
		}

		if err != nil {
			return errors.Wrap(err, "Error generating output")
		}

		if outputCfg.Path != "" {
			err := ioutil.WriteFile(outputCfg.Path, []byte(out), 0644) // nolint:gosec
			if err != nil {
				return errors.Wrap(err, "Error saving output")
			}
		} else {
			fmt.Printf("%s\n", out)
		}
	}

	return nil
}

func loadRunFlags(cfg *config.Config, cmd *cobra.Command) error {
	hasConfigFile := cmd.Flags().Changed("config-file")

	hasProjectFlags := (cmd.Flags().Changed("path") ||
		cmd.Flags().Changed("terraform-plan-flags") ||
		cmd.Flags().Changed("usage-file"))

	hasOutputFlags := (cmd.Flags().Changed("format") ||
		cmd.Flags().Changed("show-skipped"))

	if hasConfigFile && hasProjectFlags {
		ui.PrintUsageErrorAndExit(cmd, "--config-file flag cannot be used with other project and output flags")
	}

	if hasConfigFile {
		configFile, _ := cmd.Flags().GetString("config-file")
		err := cfg.LoadFromFile(configFile)

		if err != nil {
			return err
		}
	}

	projectCfg := &config.TerraformProject{}

	if hasProjectFlags {
		cfg.Projects = config.Projects{
			Terraform: []*config.TerraformProject{
				projectCfg,
			},
		}
	}

	outputCfg := &config.Output{}
	if hasOutputFlags {
		cfg.Outputs = []*config.Output{outputCfg}
	}

	if hasProjectFlags || hasOutputFlags {
		err := cfg.LoadFromEnv()
		if err != nil {
			return err
		}
	}

	if hasProjectFlags {
		projectCfg.Path, _ = cmd.Flags().GetString("path")
		projectCfg.UseState, _ = cmd.Flags().GetBool("terraform-use-state")
		projectCfg.PlanFlags, _ = cmd.Flags().GetString("terraform-plan-flags")
		projectCfg.UsageFile, _ = cmd.Flags().GetString("usage-file")
	}

	if hasOutputFlags {
		outputCfg.Format, _ = cmd.Flags().GetString("format")
		outputCfg.ShowSkipped, _ = cmd.Flags().GetBool("show-skipped")
	}

	return nil
}

func checkRunConfig(cfg *config.Config) error {
	for _, output := range cfg.Outputs {
		if output.Format == "json" && output.ShowSkipped {
			ui.PrintWarning("The show skipped option is not needed with JSON output as that always includes them.")
			return nil
		}
	}

	return nil
}

func unwrapped(err error) error {
	e := err
	for errors.Unwrap(e) != nil {
		e = errors.Unwrap(e)
	}

	return e
}
