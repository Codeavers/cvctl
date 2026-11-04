package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/images"
	"github.com/codeavers/cvctl/internal/output"
)

type imageRow struct {
	Image   string `json:"image"`
	Size    int64  `json:"size"`
	Removed bool   `json:"removed"`
	Reason  string `json:"reason"`
}

type imagesPruneReport struct {
	DryRun     bool       `json:"dry_run"`
	Items      []imageRow `json:"items"`
	TotalBytes int64      `json:"total_bytes"`
}

func newImagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "images",
		Short:   "Manage the images cvctl has deployed",
		Example: `  sudo cvctl images prune --dry-run`,
	}
	cmd.AddCommand(newImagesPruneCmd())
	return cmd
}

func newImagesPruneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove images no instance needs any more",
		Example: `  sudo cvctl images prune --dry-run
  sudo cvctl images prune
  sudo cvctl images prune --instance myapp-dev`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			only, _ := cmd.Flags().GetString("instance")
			asJSON, _ := cmd.Flags().GetBool("json")

			e, err := newOpsEnv(opsOpts{NonInteractive: asJSON, In: cmd.InOrStdin(), Out: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			if e.Deployer == nil || e.Deployer.Images == nil {
				return &output.Error{Code: "E_IMAGES_UNAVAILABLE", Message: "image cleanup is not configured"}
			}
			if only != "" {
				if err := e.checkInstance(only); err != nil {
					return err
				}
			}
			plan, err := e.Deployer.ImagePlan(only)
			if err != nil {
				return &output.Error{
					Code:    "E_IMAGES_STATE",
					Message: err.Error(),
					Hint:    "run with sudo; nothing was removed",
				}
			}
			items, err := e.Deployer.Images.Prune(cmd.Context(), plan, dryRun)
			if err != nil {
				return err
			}

			rep := buildImagesReport(items, dryRun)
			printerFrom(cmd).Result(rep, func(w io.Writer) { printImagesReport(w, rep) })
			return nil
		},
	}
	cmd.Flags().Bool("dry-run", false, "list what would be removed without removing it")
	cmd.Flags().String("instance", "", "only remove images of this instance")
	return cmd
}

func buildImagesReport(items []images.Item, dryRun bool) imagesPruneReport {
	rep := imagesPruneReport{DryRun: dryRun, Items: []imageRow{}}
	counted := map[string]bool{}
	for _, it := range items {
		reason := it.Reason
		switch {
		case dryRun:
			reason = "would remove"
		case it.Removed:
			reason = "removed"
		case it.InUse:
			reason = "in use"
		default:
			reason, _, _ = strings.Cut(reason, "\n")
		}
		rep.Items = append(rep.Items, imageRow{Image: it.Ref, Size: it.Size, Removed: it.Removed, Reason: reason})
		if !dryRun && !it.Removed {
			continue
		}
		// The size is per image, which several repositories may share.
		_, digest, _ := strings.Cut(it.Ref, "@")
		if !counted[digest] {
			counted[digest] = true
			rep.TotalBytes += it.Size
		}
	}
	return rep
}

func printImagesReport(w io.Writer, rep imagesPruneReport) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, r := range rep.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Image, formatBytes(r.Size), r.Reason)
	}
	tw.Flush()
	verb := "freed"
	if rep.DryRun {
		verb = "would free"
	}
	fmt.Fprintf(w, "total: %s %s\n", verb, formatBytes(rep.TotalBytes))
}

// formatBytes uses Docker's decimal units, the ones the sizes came in.
func formatBytes(n int64) string {
	units := []struct {
		suffix string
		size   float64
	}{{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}}
	for _, u := range units {
		if float64(n) >= u.size {
			return strconv.FormatFloat(float64(n)/u.size, 'f', 1, 64) + u.suffix
		}
	}
	return fmt.Sprintf("%dB", n)
}
