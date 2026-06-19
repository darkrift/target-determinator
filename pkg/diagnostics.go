package pkg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/analysis"
	"github.com/bazel-contrib/target-determinator/third_party/protobuf/bazel/build"
	"github.com/bazelbuild/bazel-gazelle/label"
	"google.golang.org/protobuf/proto"
)

type diagnostics struct {
	dir      string
	enabled  bool
	disabled bool
	mu       sync.Mutex
	files    map[string]*os.File
}

var activeDiagnostics = newDiagnostics()

func newDiagnostics() *diagnostics {
	dir := os.Getenv("TD_DEBUG_DIR")
	return &diagnostics{
		dir:     dir,
		enabled: dir != "",
		files:   make(map[string]*os.File),
	}
}

func diagnosticsEnabled() bool {
	return activeDiagnostics.enabled && !activeDiagnostics.disabled
}

func CloseDiagnostics() {
	activeDiagnostics.close()
}

func LogInvocationDiagnostics(context *Context, revBefore LabelledGitRev, targets TargetsList, verbose bool) {
	if diagnosticsEnabled() {
		log.Printf("TD_DIAG: writing diagnostic JSONL files to %s", activeDiagnostics.dir)
	}
	bazel := map[string]interface{}{
		"hash_key": context.BazelCmd.HashKey(),
	}
	if defaultCmd, ok := context.BazelCmd.(DefaultBazelCmd); ok {
		bazel["path"] = defaultCmd.BazelPath
		bazel["startup_opts"] = defaultCmd.BazelStartupOpts
		bazel["opts"] = defaultCmd.BazelOpts
	}
	fields := map[string]interface{}{
		"workspace_path":                context.WorkspacePath,
		"original_revision":             context.OriginalRevision.String(),
		"before_revision":               revBefore.String(),
		"target_pattern":                targets.String(),
		"verbose":                       verbose,
		"analysis_cache_clear_strategy": context.AnalysisCacheClearStrategy,
		"compare_queries_around_analysis_cache_clear": context.CompareQueriesAroundAnalysisCacheClear,
		"filter_incompatible_targets":                 context.FilterIncompatibleTargets,
		"cache_directory":                             context.CacheDirectory,
		"no_cache_results":                            context.NoCacheResults,
		"include_differences":                         context.IncludeDifferences,
		"delete_cached_worktree":                      context.DeleteCachedWorktree,
		"bazel_output_base":                           context.BazelOutputBase,
		"bazel":                                       bazel,
		"td_worker_count":                             os.Getenv("TD_WORKER_COUNT"),
		"td_debug_dir":                                os.Getenv("TD_DEBUG_DIR"),
		"td_debug_transitive_hashes":                  os.Getenv("TD_DEBUG_TRANSITIVE_HASHES"),
		"td_debug_transitive_record_limit":            os.Getenv("TD_DEBUG_TRANSITIVE_RECORD_LIMIT"),
	}
	log.Printf("Invocation: workspace=%s before=%s original=%s targets=%q verbose=%v cache_dir=%q nocache=%v",
		context.WorkspacePath, revBefore.GitRevision.String(), context.OriginalRevision.GitRevision.String(), targets.String(), verbose, context.CacheDirectory, context.NoCacheResults)
	diagEvent("invocation", fields)
}

func (d *diagnostics) close() {
	if !d.enabled {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, f := range d.files {
		if err := f.Close(); err != nil {
			log.Printf("TD_DIAG: failed to close diagnostic file %s: %v", name, err)
		}
	}
	d.files = make(map[string]*os.File)
}

func diagEvent(event string, fields map[string]interface{}) {
	if !diagnosticsEnabled() {
		return
	}
	record := map[string]interface{}{
		"event": event,
	}
	for k, v := range fields {
		record[k] = v
	}
	diagRecord("events", record)
}

func diagRecord(name string, record map[string]interface{}) {
	if !diagnosticsEnabled() {
		return
	}
	recordWithTimestamp := map[string]interface{}{
		"ts": time.Now().Format(time.RFC3339Nano),
	}
	for k, v := range record {
		recordWithTimestamp[k] = v
	}
	activeDiagnostics.writeJSONL(name, recordWithTimestamp)
}

func (d *diagnostics) writeJSONL(name string, record map[string]interface{}) {
	if !d.enabled {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.disabled {
		return
	}
	if err := os.MkdirAll(d.dir, 0755); err != nil {
		log.Printf("TD_DIAG: disabling diagnostics; failed to create %s: %v", d.dir, err)
		d.disabled = true
		return
	}
	f, ok := d.files[name]
	if !ok {
		path := filepath.Join(d.dir, name+".jsonl")
		var err error
		f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("TD_DIAG: disabling diagnostics; failed to open %s: %v", path, err)
			d.disabled = true
			return
		}
		d.files[name] = f
	}
	if err := json.NewEncoder(f).Encode(record); err != nil {
		log.Printf("TD_DIAG: failed to write %s diagnostic record: %v", name, err)
	}
}

func diagIntEnv(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		log.Printf("TD_DIAG: ignoring invalid %s=%q; using %d", name, value, fallback)
		return fallback
	}
	return parsed
}

type countEntry struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

func topStringCounts(counts map[string]int, limit int) []countEntry {
	entries := make([]countEntry, 0, len(counts))
	for k, v := range counts {
		entries = append(entries, countEntry{Key: k, Count: v})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count == entries[j].Count {
			return entries[i].Key < entries[j].Key
		}
		return entries[i].Count > entries[j].Count
	})
	if limit >= 0 && len(entries) > limit {
		return entries[:limit]
	}
	return entries
}

func hashBytesForLog(hash []byte) string {
	if hash == nil {
		return ""
	}
	return hex.EncodeToString(hash)
}

func shortHashForLog(hash []byte) string {
	full := hashBytesForLog(hash)
	if len(full) <= 16 {
		return full
	}
	return full[:16]
}

func stableProtoDigest(message proto.Message) string {
	if message == nil {
		return ""
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return "marshal-error:" + err.Error()
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func configString(configuration Configuration) string {
	return configuration.inner
}

func labelPackagePrefix(l label.Label) string {
	repoPrefix := "//"
	if l.Repo != "" {
		repoPrefix = "@" + l.Repo + "//"
	}
	if l.Pkg == "" {
		return repoPrefix
	}
	parts := strings.Split(l.Pkg, "/")
	if len(parts) > 2 {
		return repoPrefix + strings.Join(parts[:2], "/") + "/..."
	}
	return repoPrefix + l.Pkg
}

func sampleLabels(labels []label.Label, limit int) []string {
	if limit < 0 || len(labels) < limit {
		limit = len(labels)
	}
	sample := make([]string, 0, limit)
	for _, l := range labels[:limit] {
		sample = append(sample, l.String())
	}
	return sample
}

func tailForDiagnostics(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return s[len(s)-maxBytes:]
}

func matchingTargetsSummary(mt *MatchingTargets) map[string]interface{} {
	if mt == nil {
		return map[string]interface{}{
			"labels":             0,
			"configured_targets": 0,
			"nil":                true,
		}
	}
	labels := mt.Labels()
	configuredTargets := 0
	maxConfigurationsPerLabel := 0
	packageCounts := make(map[string]int)
	for _, l := range labels {
		configurations := mt.ConfigurationsFor(l)
		configuredTargets += len(configurations)
		if len(configurations) > maxConfigurationsPerLabel {
			maxConfigurationsPerLabel = len(configurations)
		}
		packageCounts[labelPackagePrefix(l)]++
	}
	return map[string]interface{}{
		"labels":                       len(labels),
		"configured_targets":           configuredTargets,
		"max_configurations_per_label": maxConfigurationsPerLabel,
		"top_packages":                 topStringCounts(packageCounts, 20),
		"sample_labels":                sampleLabels(labels, 20),
	}
}

func configuredTargetsSummary(targets map[label.Label]map[Configuration]*analysis.ConfiguredTarget) map[string]interface{} {
	if targets == nil {
		return map[string]interface{}{
			"labels":             0,
			"configured_targets": 0,
			"nil":                true,
		}
	}
	configuredTargets := 0
	maxConfigurationsPerLabel := 0
	typeCounts := make(map[string]int)
	ruleClassCounts := make(map[string]int)
	packageCounts := make(map[string]int)
	for l, configurations := range targets {
		if len(configurations) > maxConfigurationsPerLabel {
			maxConfigurationsPerLabel = len(configurations)
		}
		packageCounts[labelPackagePrefix(l)]++
		for _, configuredTarget := range configurations {
			configuredTargets++
			typeCounts[targetTypeString(configuredTarget)]++
			if ruleClass := ruleClassString(configuredTarget); ruleClass != "" {
				ruleClassCounts[ruleClass]++
			}
		}
	}
	return map[string]interface{}{
		"labels":                       len(targets),
		"configured_targets":           configuredTargets,
		"max_configurations_per_label": maxConfigurationsPerLabel,
		"top_types":                    topStringCounts(typeCounts, 20),
		"top_rule_classes":             topStringCounts(ruleClassCounts, 30),
		"top_packages":                 topStringCounts(packageCounts, 20),
	}
}

func targetTypeString(configuredTarget *analysis.ConfiguredTarget) string {
	if configuredTarget == nil || configuredTarget.GetTarget() == nil || configuredTarget.GetTarget().Type == nil {
		return ""
	}
	return configuredTarget.GetTarget().GetType().String()
}

func ruleClassString(configuredTarget *analysis.ConfiguredTarget) string {
	if configuredTarget == nil || configuredTarget.GetTarget() == nil {
		return ""
	}
	if configuredTarget.GetTarget().GetType() != build.Target_RULE {
		return ""
	}
	return configuredTarget.GetTarget().GetRule().GetRuleClass()
}

func targetNameString(configuredTarget *analysis.ConfiguredTarget) string {
	if configuredTarget == nil || configuredTarget.GetTarget() == nil {
		return ""
	}
	target := configuredTarget.GetTarget()
	switch target.GetType() {
	case build.Target_RULE:
		return target.GetRule().GetName()
	case build.Target_SOURCE_FILE:
		return target.GetSourceFile().GetName()
	case build.Target_GENERATED_FILE:
		return target.GetGeneratedFile().GetName()
	case build.Target_PACKAGE_GROUP:
		return target.GetPackageGroup().GetName()
	case build.Target_ENVIRONMENT_GROUP:
		return target.GetEnvironmentGroup().GetName()
	default:
		return ""
	}
}

func configuredTargetDiagnostic(configuredTarget *analysis.ConfiguredTarget) map[string]interface{} {
	if configuredTarget == nil {
		return map[string]interface{}{"nil": true}
	}
	target := configuredTarget.GetTarget()
	if target == nil {
		return map[string]interface{}{"nil_target": true}
	}
	record := map[string]interface{}{
		"name":              targetNameString(configuredTarget),
		"type":              targetTypeString(configuredTarget),
		"configuration":     configuredTarget.GetConfiguration().GetChecksum(),
		"target_proto_hash": stableProtoDigest(target),
	}
	if ruleClass := ruleClassString(configuredTarget); ruleClass != "" {
		record["rule_class"] = ruleClass
		record["skylark_environment_hash_code"] = target.GetRule().GetSkylarkEnvironmentHashCode()
		record["attribute_count"] = len(target.GetRule().GetAttribute())
		record["rule_input_count"] = len(target.GetRule().GetRuleInput())
		record["configured_rule_input_count"] = len(target.GetRule().GetConfiguredRuleInput())
	}
	return record
}

func differencesForDiagnostics(differences []Difference) []map[string]string {
	records := make([]map[string]string, 0, len(differences))
	for _, difference := range differences {
		records = append(records, map[string]string{
			"category": difference.Category,
			"key":      difference.Key,
			"before":   difference.Before,
			"after":    difference.After,
		})
	}
	return records
}

func matchingTargetComparisonSummary(before, after *MatchingTargets) map[string]interface{} {
	beforeLabels, beforeConfiguredTargets := matchingTargetSets(before)
	afterLabels, afterConfiguredTargets := matchingTargetSets(after)

	beforeOnlyLabels := stringSetDifference(beforeLabels, afterLabels)
	afterOnlyLabels := stringSetDifference(afterLabels, beforeLabels)
	beforeOnlyConfiguredTargets := stringSetDifference(beforeConfiguredTargets, afterConfiguredTargets)
	afterOnlyConfiguredTargets := stringSetDifference(afterConfiguredTargets, beforeConfiguredTargets)

	return map[string]interface{}{
		"before":                         matchingTargetsSummary(before),
		"after":                          matchingTargetsSummary(after),
		"common_labels":                  len(beforeLabels) - len(beforeOnlyLabels),
		"before_only_labels":             len(beforeOnlyLabels),
		"after_only_labels":              len(afterOnlyLabels),
		"before_only_configured_targets": len(beforeOnlyConfiguredTargets),
		"after_only_configured_targets":  len(afterOnlyConfiguredTargets),
		"sample_before_only_labels":      firstStrings(beforeOnlyLabels, 50),
		"sample_after_only_labels":       firstStrings(afterOnlyLabels, 50),
		"sample_before_only_configured":  firstStrings(beforeOnlyConfiguredTargets, 50),
		"sample_after_only_configured":   firstStrings(afterOnlyConfiguredTargets, 50),
	}
}

func matchingTargetSets(mt *MatchingTargets) (map[string]struct{}, map[string]struct{}) {
	labels := make(map[string]struct{})
	configuredTargets := make(map[string]struct{})
	if mt == nil {
		return labels, configuredTargets
	}
	for _, l := range mt.Labels() {
		labels[l.String()] = struct{}{}
		for _, configuration := range mt.ConfigurationsFor(l) {
			configuredTargets[configuredTargetPairKey(l, configuration)] = struct{}{}
		}
	}
	return labels, configuredTargets
}

func configuredTargetPairKey(l label.Label, configuration Configuration) string {
	return l.String() + "[" + configString(configuration) + "]"
}

func stringSetDifference(left, right map[string]struct{}) []string {
	diff := make([]string, 0)
	for item := range left {
		if _, ok := right[item]; !ok {
			diff = append(diff, item)
		}
	}
	sort.Strings(diff)
	return diff
}

func firstStrings(items []string, limit int) []string {
	if limit < 0 || len(items) < limit {
		limit = len(items)
	}
	result := make([]string, limit)
	copy(result, items[:limit])
	return result
}

type configuredTargetPair struct {
	label         label.Label
	configuration Configuration
}

func summarizeTransitiveHashDifferences(before, after *QueryResults) {
	if !diagnosticsEnabled() {
		return
	}
	if os.Getenv("TD_DEBUG_TRANSITIVE_HASHES") == "0" {
		log.Printf("TD_DIAG: skipping transitive hash diagnostics because TD_DEBUG_TRANSITIVE_HASHES=0")
		return
	}
	if before == nil || after == nil || before.TransitiveConfiguredTargets == nil || after.TransitiveConfiguredTargets == nil {
		diagEvent("transitive_hash_diff_skipped", map[string]interface{}{
			"reason": "missing transitive configured target metadata",
		})
		return
	}

	start := time.Now()
	recordLimit := diagIntEnv("TD_DEBUG_TRANSITIVE_RECORD_LIMIT", 2000)
	walkDiffLimit := diagIntEnv("TD_DEBUG_TRANSITIVE_WALKDIFF_LIMIT", 200)
	recordCount := 0
	walkDiffCount := 0

	stats := map[string]interface{}{
		"after_configured_targets":   0,
		"common_configured_targets":  0,
		"changed_hashes":             0,
		"added_configured_targets":   0,
		"removed_configured_targets": 0,
		"hash_errors":                0,
	}
	changedRuleClasses := make(map[string]int)
	changedPackages := make(map[string]int)
	changeCategories := make(map[string]int)
	hashErrorMessages := make(map[string]int)

	afterPairs := sortedConfiguredTargetPairs(after.TransitiveConfiguredTargets)
	beforePairSet := configuredTargetPairSet(before.TransitiveConfiguredTargets)
	afterPairSet := configuredTargetPairSet(after.TransitiveConfiguredTargets)

	for _, pair := range afterPairs {
		stats["after_configured_targets"] = stats["after_configured_targets"].(int) + 1
		beforeConfiguredTarget := configuredTargetFor(before, pair.label, pair.configuration)
		afterConfiguredTarget := configuredTargetFor(after, pair.label, pair.configuration)
		if beforeConfiguredTarget == nil {
			stats["added_configured_targets"] = stats["added_configured_targets"].(int) + 1
			if recordCount < recordLimit {
				diagRecord("transitive_hash_diffs", map[string]interface{}{
					"label":         pair.label.String(),
					"configuration": configString(pair.configuration),
					"reason":        "AddedTransitiveConfiguredTarget",
					"after_target":  configuredTargetDiagnostic(afterConfiguredTarget),
				})
				recordCount++
			}
			continue
		}
		stats["common_configured_targets"] = stats["common_configured_targets"].(int) + 1
		labelAndConfiguration := LabelAndConfiguration{
			Label:         pair.label,
			Configuration: pair.configuration,
		}
		beforeHash, beforeErr := before.TargetHashCache.Hash(labelAndConfiguration)
		afterHash, afterErr := after.TargetHashCache.Hash(labelAndConfiguration)
		if beforeErr != nil || afterErr != nil {
			stats["hash_errors"] = stats["hash_errors"].(int) + 1
			if beforeErr != nil {
				hashErrorMessages[beforeErr.Error()]++
			}
			if afterErr != nil {
				hashErrorMessages[afterErr.Error()]++
			}
			if recordCount < recordLimit {
				record := map[string]interface{}{
					"label":         pair.label.String(),
					"configuration": configString(pair.configuration),
					"reason":        "HashError",
					"before_target": configuredTargetDiagnostic(beforeConfiguredTarget),
					"after_target":  configuredTargetDiagnostic(afterConfiguredTarget),
				}
				if beforeErr != nil {
					record["before_error"] = beforeErr.Error()
				}
				if afterErr != nil {
					record["after_error"] = afterErr.Error()
				}
				diagRecord("transitive_hash_diffs", record)
				recordCount++
			}
			continue
		}
		if bytes.Equal(beforeHash, afterHash) {
			continue
		}
		stats["changed_hashes"] = stats["changed_hashes"].(int) + 1
		changedPackages[labelPackagePrefix(pair.label)]++
		if ruleClass := ruleClassString(afterConfiguredTarget); ruleClass != "" {
			changedRuleClasses[ruleClass]++
		}

		var differences []Difference
		var differenceError string
		if walkDiffCount < walkDiffLimit {
			walkDiffCount++
			var err error
			differences, err = WalkDiffs(before.TargetHashCache, after.TargetHashCache, labelAndConfiguration)
			if err != nil {
				differenceError = err.Error()
			}
			for _, difference := range differences {
				changeCategories[difference.Category]++
			}
		}
		if recordCount < recordLimit {
			diagRecord("transitive_hash_diffs", map[string]interface{}{
				"label":            pair.label.String(),
				"configuration":    configString(pair.configuration),
				"reason":           "HashChanged",
				"before_hash":      hashBytesForLog(beforeHash),
				"after_hash":       hashBytesForLog(afterHash),
				"differences":      differencesForDiagnostics(differences),
				"difference_error": differenceError,
				"before_target":    configuredTargetDiagnostic(beforeConfiguredTarget),
				"after_target":     configuredTargetDiagnostic(afterConfiguredTarget),
			})
			recordCount++
		}
	}

	removedRecords := 0
	for key := range beforePairSet {
		if _, ok := afterPairSet[key]; !ok {
			stats["removed_configured_targets"] = stats["removed_configured_targets"].(int) + 1
			if recordCount < recordLimit && removedRecords < 100 {
				diagRecord("transitive_hash_diffs", map[string]interface{}{
					"configured_target": key,
					"reason":            "RemovedTransitiveConfiguredTarget",
				})
				recordCount++
				removedRecords++
			}
		}
	}

	stats["elapsed"] = time.Since(start).String()
	stats["record_limit"] = recordLimit
	stats["records_written"] = recordCount
	stats["walkdiff_limit"] = walkDiffLimit
	stats["walkdiffs_computed"] = walkDiffCount
	stats["top_changed_rule_classes"] = topStringCounts(changedRuleClasses, 50)
	stats["top_changed_packages"] = topStringCounts(changedPackages, 50)
	stats["sample_difference_categories"] = topStringCounts(changeCategories, 50)
	stats["top_hash_errors"] = topStringCounts(hashErrorMessages, 20)

	log.Printf("Transitive hash diff summary: %v", stats)
	diagEvent("transitive_hash_diff_summary", stats)
}

func sortedConfiguredTargetPairs(targets map[label.Label]map[Configuration]*analysis.ConfiguredTarget) []configuredTargetPair {
	pairs := make([]configuredTargetPair, 0)
	for l, configurations := range targets {
		for configuration := range configurations {
			pairs = append(pairs, configuredTargetPair{label: l, configuration: configuration})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		leftLabel := pairs[i].label.String()
		rightLabel := pairs[j].label.String()
		if leftLabel == rightLabel {
			return configString(pairs[i].configuration) < configString(pairs[j].configuration)
		}
		return leftLabel < rightLabel
	})
	return pairs
}

func configuredTargetPairSet(targets map[label.Label]map[Configuration]*analysis.ConfiguredTarget) map[string]struct{} {
	set := make(map[string]struct{})
	for l, configurations := range targets {
		for configuration := range configurations {
			set[configuredTargetPairKey(l, configuration)] = struct{}{}
		}
	}
	return set
}
