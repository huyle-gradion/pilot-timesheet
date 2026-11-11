package timesheet

import "strings"

type ConfiguredDefaultClassification struct {
	CatalogID        string `json:"catalog_id"`
	ClassificationID string `json:"classification_id"`
	Code             string `json:"code"`
	Name             string `json:"name"`
	ActivityType     string `json:"activity_type"`
}

func (d *ConfiguredDefaultClassification) normalize() error {
	for _, field := range []*string{&d.CatalogID, &d.ClassificationID, &d.Code, &d.Name, &d.ActivityType} {
		*field = strings.TrimSpace(*field)
		if *field == "" {
			return newError(CodeConfigInvalid, "config: gradion.default_classification requires catalog_id, classification_id, code, name and activity_type")
		}
	}
	return nil
}

func configuredDefaultCatalogID(cfg *Config) string {
	if cfg.Gradion != nil && cfg.Gradion.DefaultClassification != nil {
		return cfg.Gradion.DefaultClassification.CatalogID
	}
	return ""
}

func resolveConfiguredDefault(cfg *Config, state *State) (ClassificationChoiceEntry, error) {
	if cfg.Gradion == nil || !cfg.Gradion.Enabled {
		return ClassificationChoiceEntry{}, newError(CodeRemoteDisabled, "configured default requires gradion.enabled")
	}
	d := cfg.Gradion.DefaultClassification
	if d == nil {
		return ClassificationChoiceEntry{}, newError(CodeConfigInvalid, "configure gradion.default_classification with a reviewed catalog binding first")
	}
	if state.Remote == nil || state.Remote.Catalogs[d.CatalogID] == nil {
		return ClassificationChoiceEntry{}, newError(CodeStaleCatalog, "configured default catalog is not cached; explicitly run classifications and review/update gradion.default_classification, or use classify --file")
	}
	cat := state.Remote.Catalogs[d.CatalogID]
	var matches int
	for _, option := range cat.Options {
		if option.ID != d.ClassificationID {
			continue
		}
		matches++
		if choiceFromOption(option) != (ClassificationChoice{ID: d.ClassificationID, Code: d.Code, Name: d.Name, ActivityType: d.ActivityType}) {
			return ClassificationChoiceEntry{}, newError(CodeClassificationChanged, "configured default semantics do not match the cached option; review/update gradion.default_classification or use classify --file")
		}
	}
	if cat.CatalogID != d.CatalogID || matches != 1 {
		return ClassificationChoiceEntry{}, newError(CodeStaleCatalog, "configured default does not identify exactly one cached option; review/update gradion.default_classification or use classify --file")
	}
	return ClassificationChoiceEntry{CatalogID: d.CatalogID, ClassificationID: d.ClassificationID}, nil
}

// ConfiguredDefault resolves only the cached binding, without remote discovery
// or exposing mutable store state.
func (s *Session) ConfiguredDefault() (ClassificationChoiceEntry, error) {
	return resolveConfiguredDefault(s.cfg, s.store.State())
}
