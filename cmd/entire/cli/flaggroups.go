package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// flagGroupAnnotation marks which help group a flag renders under; see
// useGroupedFlagHelp.
const flagGroupAnnotation = "entire_flag_group"

// Shared flag-group names, so every list command presents the same taxonomy:
// how much is fetched and which page (navigation), what the client narrows or
// orders after the fetch (filtering & sorting), and how output is rendered
// (formatting).
const (
	flagGroupNavigation = "Navigation"
	flagGroupFiltering  = "Filtering & Sorting"
	flagGroupFormatting = "Formatting"
)

// setFlagGroup assigns the named local flags to a help group. Naming a flag
// that does not exist is a programming error and panics at wiring time.
func setFlagGroup(cmd *cobra.Command, group string, names ...string) {
	for _, n := range names {
		if err := cmd.Flags().SetAnnotation(n, flagGroupAnnotation, []string{group}); err != nil {
			panic(fmt.Sprintf("flag group %q: %v", group, err))
		}
	}
}

// useGroupedFlagHelp replaces the command's flat "Flags:" usage section with
// one "<Group> Flags:" section per group, in the given order. Ungrouped
// visible local flags (e.g. help) render under a plain "Flags:" section after
// the groups; inherited flags keep their usual "Global Flags:" section.
func useGroupedFlagHelp(cmd *cobra.Command, order ...string) {
	cmd.SetUsageFunc(func(c *cobra.Command) error {
		w := c.OutOrStderr()
		fmt.Fprintf(w, "Usage:\n  %s\n", c.UseLine())
		groups := make(map[string]*pflag.FlagSet, len(order)+1)
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden {
				return
			}
			group := ""
			if a := f.Annotations[flagGroupAnnotation]; len(a) > 0 {
				group = a[0]
			}
			fs, ok := groups[group]
			if !ok {
				fs = pflag.NewFlagSet(group, pflag.ContinueOnError)
				groups[group] = fs
			}
			fs.AddFlag(f)
		})
		for _, name := range order {
			if fs, ok := groups[name]; ok {
				fmt.Fprintf(w, "\n%s Flags:\n%s", name, fs.FlagUsages())
			}
		}
		if fs, ok := groups[""]; ok {
			fmt.Fprintf(w, "\nFlags:\n%s", fs.FlagUsages())
		}
		if c.HasAvailableInheritedFlags() {
			fmt.Fprintf(w, "\nGlobal Flags:\n%s", c.InheritedFlags().FlagUsages())
		}
		return nil
	})
}
