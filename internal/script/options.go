package script

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// Option describes one daemon flag a runtime plugin declares. It is the
// mechanism by which a plugin's administrator-facing knobs bubble up into
// `mcwsd daemon --help`: core reads the plugin's `options` export and
// registers one cobra flag per entry, so the help text shows exactly the
// flags the selected plugin understands.
type Option struct {
	// Flag is the long flag name without the leading dashes, e.g.
	// "jail-interface". It is registered as --<flag> on the daemon command.
	Flag string `json:"flag"`

	// Type is one of "string", "int", "bool", or "duration".
	Type string `json:"type"`

	// Default is the string form of the default value. Empty string is
	// valid; a bool option defaults to false unless Default is "true".
	Default string `json:"default,omitempty"`

	// Usage is the one-line help text shown in --help.
	Usage string `json:"usage"`
}

// validateOption reports a problem with a plugin-declared option so a typo in
// the schema surfaces at startup rather than as a confusing flag error later.
func validateOption(o Option) error {
	if strings.TrimSpace(o.Flag) == "" {
		return fmt.Errorf("option flag is required")
	}
	if strings.ContainsAny(o.Flag, " \t\n") {
		return fmt.Errorf("option flag %q must not contain whitespace", o.Flag)
	}
	switch o.Type {
	case "string", "int", "bool", "duration":
	default:
		return fmt.Errorf("option %q has unsupported type %q (want string, int, bool, or duration)", o.Flag, o.Type)
	}
	if o.Type == "bool" && o.Default != "" && o.Default != "true" && o.Default != "false" {
		return fmt.Errorf("option %q is a bool but has non-boolean default %q", o.Flag, o.Default)
	}
	return nil
}

func parseDurationDefault(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// BindFlags registers each option on fs and returns a resolver that reads the
// parsed values back out. A duplicate flag name (two options, or a collision
// with a core flag) is an error rather than a silent last-wins.
func BindFlags(fs *pflag.FlagSet, options []Option) (*OptionValues, error) {
	vals := &OptionValues{flags: make(map[string]*pflag.Flag, len(options)), order: make([]string, 0, len(options))}

	for _, o := range options {
		if err := validateOption(o); err != nil {
			return nil, err
		}
		if fs.Lookup(o.Flag) != nil {
			return nil, fmt.Errorf("plugin option %q collides with an existing flag", o.Flag)
		}
		switch o.Type {
		case "string":
			fs.String(o.Flag, o.Default, o.Usage)
		case "int":
			d, _ := strconv.Atoi(o.Default)
			fs.Int(o.Flag, d, o.Usage)
		case "bool":
			d := o.Default == "true"
			fs.Bool(o.Flag, d, o.Usage)
		case "duration":
			fs.Duration(o.Flag, parseDurationDefault(o.Default), o.Usage)
		}
		vals.flags[o.Flag] = fs.Lookup(o.Flag)
		vals.order = append(vals.order, o.Flag)
	}
	return vals, nil
}

// OptionValues holds the parsed values of a plugin's declared options.
type OptionValues struct {
	flags map[string]*pflag.Flag
	order []string
}

// Object returns the resolved options as a map suitable for handing to the
// plugin's host as `host.options`. Types are Go-native (string, int64, bool,
// float64 seconds for durations) so JSON/JS sees them naturally.
func (v *OptionValues) Object() map[string]interface{} {
	out := make(map[string]interface{}, len(v.flags))
	for _, name := range v.order {
		f := v.flags[name]
		if f == nil {
			continue
		}
		switch f.Value.Type() {
		case "bool":
			b, _ := strconv.ParseBool(f.Value.String())
			out[name] = b
		case "int":
			n, _ := strconv.ParseInt(f.Value.String(), 10, 64)
			out[name] = n
		case "duration":
			d, _ := time.ParseDuration(f.Value.String())
			out[name] = d.Seconds()
		default:
			out[name] = f.Value.String()
		}
	}
	return out
}

// SortedNames returns the flag names in registration order.
func (v *OptionValues) SortedNames() []string {
	s := append([]string(nil), v.order...)
	sort.Strings(s)
	return s
}
