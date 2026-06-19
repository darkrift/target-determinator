package pkg

import (
	"bytes"
	"fmt"
	"log"

	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/analysis"
	"github.com/bazelbuild/bazel-gazelle/label"
)

// WalkCallback is called once per affected (label, configuration) pair.
// differences explains why the target was affected; it is nil when includeDifferences is false.
// configuredTarget is the "after" ConfiguredTarget proto; it may be nil when results are
// served from cache (i.e. when the -cache-dir flag is used without -verbose).
type WalkCallback func(label.Label, []Difference, *analysis.ConfiguredTarget)

type targetComparisonDiagnostic struct {
	Label                  label.Label
	Configuration          Configuration
	Affected               bool
	Reason                 string
	BeforeHash             []byte
	AfterHash              []byte
	Differences            []Difference
	BeforeConfiguredTarget *analysis.ConfiguredTarget
	AfterConfiguredTarget  *analysis.ConfiguredTarget
}

type targetComparisonObserver func(targetComparisonDiagnostic)

type walkDiagnosticsStats struct {
	ExaminedConfiguredTargets  int
	UnchangedConfiguredTargets int
	AffectedConfiguredTargets  int
	AffectedLabels             map[string]struct{}
	Reasons                    map[string]int
	DifferenceCategories       map[string]int
	RuleInputChanged           map[string]int
}

func newWalkDiagnosticsStats() *walkDiagnosticsStats {
	return &walkDiagnosticsStats{
		AffectedLabels:       make(map[string]struct{}),
		Reasons:              make(map[string]int),
		DifferenceCategories: make(map[string]int),
		RuleInputChanged:     make(map[string]int),
	}
}

func (s *walkDiagnosticsStats) record(record targetComparisonDiagnostic) {
	s.ExaminedConfiguredTargets++
	if !record.Affected {
		s.UnchangedConfiguredTargets++
		return
	}
	s.AffectedConfiguredTargets++
	s.AffectedLabels[record.Label.String()] = struct{}{}
	s.Reasons[record.Reason]++
	for _, difference := range record.Differences {
		s.DifferenceCategories[difference.Category]++
		if difference.Category == "RuleInputChanged" {
			s.RuleInputChanged[difference.Key]++
		}
	}
}

func (s *walkDiagnosticsStats) fields() map[string]interface{} {
	return map[string]interface{}{
		"examined_configured_targets":  s.ExaminedConfiguredTargets,
		"unchanged_configured_targets": s.UnchangedConfiguredTargets,
		"affected_configured_targets":  s.AffectedConfiguredTargets,
		"affected_labels":              len(s.AffectedLabels),
		"reasons":                      s.Reasons,
		"difference_categories":        s.DifferenceCategories,
		"top_rule_input_changed":       topStringCounts(s.RuleInputChanged, 50),
	}
}

// WalkAffectedTargets computes which targets have changed between two commits, and calls
// callback once for each target which has changed.
// Explanation of the differences may be expensive in both time and memory to compute, so if
// includeDifferences is set to false, the []Difference parameter to the callback will always be nil.
func WalkAffectedTargets(context *Context, revBefore LabelledGitRev, targets TargetsList, includeDifferences bool, callback WalkCallback) error {
	// The revAfter revision represents the current state of the working directory, which may contain local changes.
	// It is distinct from context.OriginalRevision, which represents the original commit that we want to reset to before exiting.
	revAfter, err := NewLabelledGitRev(context.WorkspacePath, "", "after")
	if err != nil {
		return fmt.Errorf("could not create \"after\" revision: %w", err)
	}

	beforeMetadata, afterMetadata, err := FullyProcess(context, revBefore, revAfter, targets)
	if err != nil {
		return fmt.Errorf("failed to process change: %w", err)
	}
	log.Printf("Matching target comparison summary: %v", matchingTargetComparisonSummary(beforeMetadata.MatchingTargets, afterMetadata.MatchingTargets))
	diagEvent("matching_target_comparison", matchingTargetComparisonSummary(beforeMetadata.MatchingTargets, afterMetadata.MatchingTargets))

	if beforeMetadata.BazelRelease == afterMetadata.BazelRelease && beforeMetadata.BazelRelease == "development version" {
		log.Printf("WARN: Bazel was detected to be a development version - if you're using different development versions at the before and after commits, differences between those versions may not be reflected in this output")
	}

	stats := newWalkDiagnosticsStats()
	observer := func(record targetComparisonDiagnostic) {
		stats.record(record)
		if record.Affected {
			diagRecord("affected_targets", map[string]interface{}{
				"label":         record.Label.String(),
				"configuration": configString(record.Configuration),
				"reason":        record.Reason,
				"before_hash":   hashBytesForLog(record.BeforeHash),
				"after_hash":    hashBytesForLog(record.AfterHash),
				"differences":   differencesForDiagnostics(record.Differences),
				"before_target": configuredTargetDiagnostic(record.BeforeConfiguredTarget),
				"after_target":  configuredTargetDiagnostic(record.AfterConfiguredTarget),
			})
		}
	}

	for _, l := range afterMetadata.MatchingTargets.Labels() {
		if err := diffSingleLabel(beforeMetadata, afterMetadata, includeDifferences, l, callback, observer); err != nil {
			return err
		}
	}

	log.Printf("Affected target comparison summary: %v", stats.fields())
	diagEvent("affected_walk_summary", stats.fields())
	summarizeTransitiveHashDifferences(beforeMetadata, afterMetadata)

	return nil
}

func DiffSingleLabel(beforeMetadata, afterMetadata *QueryResults, includeDifferences bool, label label.Label, callback WalkCallback) error {
	return diffSingleLabel(beforeMetadata, afterMetadata, includeDifferences, label, callback, nil)
}

func diffSingleLabel(beforeMetadata, afterMetadata *QueryResults, includeDifferences bool, label label.Label, callback WalkCallback, observer targetComparisonObserver) error {
	for _, configuration := range afterMetadata.MatchingTargets.ConfigurationsFor(label) {
		configuredTarget := configuredTargetFor(afterMetadata, label, configuration)

		var differences []Difference

		collectDifference := func(d Difference) {
			if includeDifferences {
				differences = append(differences, d)
			}
		}

		if len(beforeMetadata.MatchingTargets.ConfigurationsFor(label)) == 0 {
			category := "NewLabel"
			if beforeMetadata.QueryError != nil {
				category = "ErrorInQueryBefore"
			}
			collectDifference(Difference{
				Category: category,
			})
			observeComparison(observer, targetComparisonDiagnostic{
				Label:                  label,
				Configuration:          configuration,
				Affected:               true,
				Reason:                 category,
				Differences:            differences,
				BeforeConfiguredTarget: configuredTargetFor(beforeMetadata, label, configuration),
				AfterConfiguredTarget:  configuredTarget,
			})
			callback(label, differences, configuredTarget)
			return nil
		} else if !beforeMetadata.MatchingTargets.ContainsLabelAndConfiguration(label, configuration) {
			difference := Difference{
				Category: "NewConfiguration",
			}
			if includeDifferences {
				configurationsBefore := beforeMetadata.MatchingTargets.ConfigurationsFor(label)
				configurationsAfter := afterMetadata.MatchingTargets.ConfigurationsFor(label)
				if len(configurationsBefore) == 1 && len(configurationsAfter) == 1 {
					diff, _ := diffConfigurations(beforeMetadata.configurations[configurationsBefore[0]], afterMetadata.configurations[configurationsAfter[0]])
					difference = Difference{
						Category: "ChangedConfiguration",
						Before:   configurationsBefore[0].String(),
						After:    configurationsAfter[0].String(),
						Key:      diff,
					}
				}
			}
			collectDifference(difference)
			observeComparison(observer, targetComparisonDiagnostic{
				Label:                  label,
				Configuration:          configuration,
				Affected:               true,
				Reason:                 difference.Category,
				Differences:            differences,
				BeforeConfiguredTarget: configuredTargetFor(beforeMetadata, label, configuration),
				AfterConfiguredTarget:  configuredTarget,
			})
			callback(label, differences, configuredTarget)
			return nil
		}
		labelAndConfiguration := LabelAndConfiguration{
			Label:         label,
			Configuration: configuration,
		}

		hashBefore, err := beforeMetadata.TargetHashCache.Hash(labelAndConfiguration)
		if err != nil {
			return err
		}
		hashAfter, err := afterMetadata.TargetHashCache.Hash(labelAndConfiguration)
		if err != nil {
			return err
		}
		if bytes.Equal(hashBefore, hashAfter) {
			observeComparison(observer, targetComparisonDiagnostic{
				Label:                  label,
				Configuration:          configuration,
				Affected:               false,
				Reason:                 "UnchangedHash",
				BeforeHash:             hashBefore,
				AfterHash:              hashAfter,
				BeforeConfiguredTarget: configuredTargetFor(beforeMetadata, label, configuration),
				AfterConfiguredTarget:  configuredTarget,
			})
			continue
		}
		if includeDifferences {
			differences, err = WalkDiffs(beforeMetadata.TargetHashCache, afterMetadata.TargetHashCache, labelAndConfiguration)
			if err != nil {
				return err
			}
		}
		observeComparison(observer, targetComparisonDiagnostic{
			Label:                  label,
			Configuration:          configuration,
			Affected:               true,
			Reason:                 "HashChanged",
			BeforeHash:             hashBefore,
			AfterHash:              hashAfter,
			Differences:            differences,
			BeforeConfiguredTarget: configuredTargetFor(beforeMetadata, label, configuration),
			AfterConfiguredTarget:  configuredTarget,
		})
		callback(label, differences, configuredTarget)
	}
	return nil
}

func observeComparison(observer targetComparisonObserver, record targetComparisonDiagnostic) {
	if observer != nil {
		observer(record)
	}
}

func configuredTargetFor(queryResults *QueryResults, l label.Label, configuration Configuration) *analysis.ConfiguredTarget {
	if queryResults == nil || queryResults.TransitiveConfiguredTargets == nil {
		return nil
	}
	configurations := queryResults.TransitiveConfiguredTargets[l]
	if configurations == nil {
		return nil
	}
	return configurations[configuration]
}
