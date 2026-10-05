package metrics

import (
	"errors"
	"sort"
	"strings"
)

// Raw facts remain the oracle. This variant adds only compact reference/anchor
// metadata needed when older facts have been folded into historical summaries.
func retainedUserSQL() string {
	sql := strings.Replace(attributionSQL, "AND received_at<=$4", "AND received_at<=$4 AND user_id=$5", 1)
	sql = strings.Replace(sql, "FROM accepted WHERE kind='exposure'", `FROM (
 SELECT user_id,variant_id,event_id,occurred_at FROM accepted WHERE kind='exposure'
 UNION ALL SELECT user_id,variant_id,event_id,occurred_at FROM metric_archived_anchors
 WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$5 AND occurred_at<=$4
 ) anchor_sources`, 1)
	return strings.Replace(sql, "LEFT JOIN raw_events x", `LEFT JOIN (
 SELECT project_id,environment_id,event_id,run_id,user_id,kind,status,variant_id,occurred_at,received_at FROM raw_events
 UNION ALL SELECT r.project_id,r.environment_id,r.event_id,r.run_id,r.user_id,r.kind,r.status,r.variant_id,r.occurred_at,r.received_at
 FROM metric_event_references r WHERE NOT EXISTS(SELECT 1 FROM raw_events f
 WHERE f.project_id=r.project_id AND f.environment_id=r.environment_id AND f.event_id=r.event_id)
 ) x`, 1)
}

// Each input belongs to the same single run/user. Requests and quality are
// additive across disjoint receipt intervals, but exposure/conversion are
// unique-user facts. If both intervals converted, the second numerator becomes
// a duplicate completion instead of a second business conversion.
func mergeHistorical(history, current derived) (derived, error) {
	result := derived{}
	if history.Quality.FutureEvents != 0 || history.Quality.PendingOutcomes != 0 {
		return result, errors.New("historical contribution contains unresolved time or references")
	}
	cohorts := make(map[string]cohortCounts)
	var historyConverted, currentConverted int64
	for _, c := range history.Cohorts {
		if !c.Finalized || c.Exposed < 0 || c.Exposed > 1 || c.Converted < 0 || c.Converted > c.Exposed {
			return result, errors.New("invalid finalized historical cohort")
		}
		cohorts[c.VariantID] = c
		historyConverted += c.Converted
	}
	for _, c := range current.Cohorts {
		if c.Exposed < 0 || c.Exposed > 1 || c.Converted < 0 || c.Converted > c.Exposed {
			return result, errors.New("invalid retained user cohort")
		}
		currentConverted += c.Converted
		if old, ok := cohorts[c.VariantID]; ok {
			if !c.Finalized {
				return result, errors.New("historical anchor became provisional")
			}
			c.Exposed = max(c.Exposed, old.Exposed)
			c.Converted = max(c.Converted, old.Converted)
		}
		cohorts[c.VariantID] = c
	}
	var exposed int64
	for _, c := range cohorts {
		result.Cohorts = append(result.Cohorts, c)
		exposed += c.Exposed
	}
	if exposed > 1 {
		return derived{}, errors.New("historical attribution anchor changed variant")
	}
	sort.Slice(result.Cohorts, func(i, j int) bool { return result.Cohorts[i].VariantID < result.Cohorts[j].VariantID })
	requests := make(map[string]requestCounts)
	for _, input := range []derived{history, current} {
		for _, r := range input.Requests {
			old := requests[r.VariantID]
			old.VariantID = r.VariantID
			old.Count += r.Count
			old.Errors += r.Errors
			requests[r.VariantID] = old
		}
	}
	for _, r := range requests {
		result.Requests = append(result.Requests, r)
	}
	sort.Slice(result.Requests, func(i, j int) bool { return result.Requests[i].VariantID < result.Requests[j].VariantID })
	type bin struct {
		variant string
		upper   int
	}
	bins := make(map[bin]int64)
	for _, input := range []derived{history, current} {
		for _, b := range input.Buckets {
			bins[bin{b.VariantID, b.UpperBoundMS}] += b.Count
		}
	}
	for k, n := range bins {
		result.Buckets = append(result.Buckets, bucketCounts{k.variant, Bucket{k.upper, n}})
	}
	sort.Slice(result.Buckets, func(i, j int) bool {
		a, b := result.Buckets[i], result.Buckets[j]
		if a.VariantID != b.VariantID {
			return a.VariantID < b.VariantID
		}
		return a.UpperBoundMS < b.UpperBoundMS
	})
	a, b := history.Quality, current.Quality
	result.Quality = Quality{a.QuarantinedEvents + b.QuarantinedEvents, a.FutureEvents + b.FutureEvents, a.PendingOutcomes + b.PendingOutcomes, a.InvalidReferenceOutcomes + b.InvalidReferenceOutcomes, a.OutsideWindowCompletions + b.OutsideWindowCompletions, a.DuplicateAttributedCompletions + b.DuplicateAttributedCompletions}
	if historyConverted > 0 && currentConverted > 0 {
		result.Quality.DuplicateAttributedCompletions++
	}
	return result, nil
}
