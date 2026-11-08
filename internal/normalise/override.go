package normalise

import (
	"slices"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

func or(a, b *string) *string {
	if a != nil && *a != "" {
		return a
	}
	return b
}

// ApplyOverride merges a systems: entry (name, tags, group, owner, dwservice_url).
func ApplyOverride(s model.System, o *config.SystemOverride) model.System {
	if o == nil {
		return s
	}
	if o.Name != "" {
		s.Name = o.Name
	}
	tags := slices.Clone(s.Tags)
	for _, t := range o.Tags {
		if !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	if tags == nil {
		tags = []string{}
	}
	s.Tags = tags
	s.Group = or(o.Group, s.Group)
	s.Owner = or(o.Owner, s.Owner)
	if o.DWServiceURL != "" {
		s.DWServiceURL = model.P(o.DWServiceURL)
	}
	return s
}
