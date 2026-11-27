package logstore

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/maximhq/bifrost/framework/queryscope"
)

// Metadata-key dimensions: grouping by one caller-supplied key inside the log
// row's metadata JSON object ("which origin / bot / tenant did this spend come
// from"). The data has always been on the row - x-bf-lh-* and x-bf-dim-*
// headers and virtual-key metadata all land in the metadata column at write
// time (see plugins/logging); this only teaches the aggregate readers to
// GROUP BY a key in it, the way jsonfielddimensions.go taught them to group
// by error classification. There is no migration or backfill behind any of
// this either.
//
// The trust model is the one metadataValueCondition (rdb.go) already uses:
// the key is validated against isValidMetadataKey (alphanumeric, dots,
// hyphens, underscores only - no quotes, so it can never break out of the SQL
// string literal it is spliced into), and everything attacker-controlled
// beyond the key - the metadata content itself - is data the SQL functions
// read, never text that enters the statement.
//
// Semantics differ from the JSON-field dimensions in two deliberate ways:
//
//   - Rows without the key (or with a non-string value) collapse into the
//     synthetic unassigned bucket instead of being excluded. These dimensions
//     exist for attribution - a cost rollup whose groups do not sum to the
//     window's total is a bug report waiting to happen - where a missing
//     error_type genuinely means "not applicable". The one exception is the
//     empty string, which also lands in unassigned: it carries no more
//     information than absence.
//   - Only string values group. Every write path (headers, dimensions,
//     virtual-key metadata) is string-valued, and string-only extraction is
//     what the three dialects can express with identical results: a number
//     would render as '42' in Postgres, 42 in SQLite and '' in ClickHouse.
//     Non-string values count as absent.
//
// Cardinality is the one cost caller-controlled values can inflate: grouping
// materializes one group per distinct value. Rankings are bounded by the
// existing RankingLimit; histograms collapse every value outside the window's
// top maxMetadataDimensionHistogramValues into a synthetic "other" series in
// Go, after the query - the database still groups fully (one scan, no extra
// pass), only the response is bounded.

const (
	// MetadataDimensionPrefix prefixes the metadata key in the compound
	// dimension value: dimension=metadata_origin groups by metadata.origin.
	// It matches the metadata_<key> filter parameter prefix deliberately -
	// filter and grouping vocabulary stay one language.
	MetadataDimensionPrefix = "metadata_"

	// maxMetadataDimensionHistogramValues caps how many distinct values a
	// metadata-key histogram names per window; the rest merge into
	// metadataDimensionOtherValue.
	maxMetadataDimensionHistogramValues = 25

	// metadataDimensionOtherValue is the series every non-top value merges
	// into in a metadata-key histogram. A caller could write the same literal
	// as a real metadata value; the merge that causes is harmless - the
	// combined series is the honest sum of both.
	metadataDimensionOtherValue = otherDimensionID
)

// ParseMetadataDimension reads the compound dimension form metadata_<key> and
// returns the key it groups by. ok is false when the dimension is not the
// compound form, the key fails isValidMetadataKey, or the key is the system's
// rather than the caller's (isAsyncRequest, bifrost_alb_*): those are excluded
// from GetDistinctMetadataKeys for being internal records, and aggregation
// keeps the same boundary.
func ParseMetadataDimension(dimension string) (key string, ok bool) {
	key, found := strings.CutPrefix(dimension, MetadataDimensionPrefix)
	if !found || !isValidMetadataKey(key) || isMetadataSystemKey(key) {
		return "", false
	}
	return key, true
}

// metadataExtractExpr returns a SQL expression yielding the string value of
// one metadata key, or SQL NULL ('' on ClickHouse) when the column is absent,
// malformed, has no such key, or the value is not a string - every one of
// those is "this row is unattributed here", never an error, matching the
// malformed-JSON discipline jsonObjectFieldExpr documents. key must already
// have passed ParseMetadataDimension; its charset is what makes splicing it
// into SQL text safe.
func metadataExtractExpr(dialect, key string) (string, bool) {
	switch dialect {
	case "postgres":
		// jsonb_typeof's argument is the -> fetch (NULL when the key is
		// absent), so the type test and the extraction share the one key
		// lookup; the metadata::jsonb cast itself is the dominant cost and
		// appears twice, the same shape the error_details dimensions already
		// accept in jsonObjectFieldExpr. The IS JSON OBJECT guard matches the
		// partial GIN index predicate the metadata filters are built around.
		return fmt.Sprintf(
			`(CASE WHEN metadata IS NOT NULL AND metadata IS JSON OBJECT AND jsonb_typeof(metadata::jsonb -> '%[1]s') = 'string' THEN metadata::jsonb ->> '%[1]s' ELSE NULL END)`,
			key,
		), true
	case "sqlite":
		// The quoted member name keeps dots and hyphens inside the key, as in
		// metadataValueCondition; json_type is NULL for an absent path, so the
		// test fails closed into ELSE without json_extract ever throwing.
		return fmt.Sprintf(
			`(CASE WHEN metadata IS NOT NULL AND json_valid(metadata) AND json_type(metadata, '$."%[1]s"') = 'text' THEN json_extract(metadata, '$."%[1]s"') ELSE NULL END)`,
			key,
		), true
	case "clickhouse":
		// JSONExtractString is not string-only in practice: current
		// ClickHouse renders numbers, booleans and even objects/arrays as
		// their JSON text rather than yielding '', so the strict string
		// semantics need the explicit JSONType test. JSONType fails closed -
		// it returns Null for malformed JSON, a missing key and every
		// non-string value, all of which land in the same "absent" outcome.
		// ClickHouse strings have no NULL, and '' is exactly the absent
		// marker the bucketing expression below collapses.
		return fmt.Sprintf(
			`if(isValidJSON(ifNull(metadata, '')) AND JSONType(ifNull(metadata, ''), '%[1]s') = 'String', JSONExtractString(ifNull(metadata, ''), '%[1]s'), '')`,
			key,
		), true
	}
	return "", false
}

// metadataGroupExpr returns the grouping expression for a metadata dimension:
// the extraction with absent and empty values collapsed into the synthetic
// unassigned bucket, so the groups are an additive split of the window's
// traffic. key must already have passed ParseMetadataDimension.
func metadataGroupExpr(dialect, key string) (string, bool) {
	extract, ok := metadataExtractExpr(dialect, key)
	if !ok {
		return "", false
	}
	if dialect == "clickhouse" {
		// The extraction never returns NULL here, so a plain empty-string
		// test is the whole bucketing.
		return fmt.Sprintf(`if(%[1]s = '', '%[2]s', %[1]s)`, extract, unassignedDimensionID), true
	}
	// The extraction appears once inside NULLIF; COALESCE then maps both
	// absent (NULL) and empty to the bucket id. COALESCE + NULLIF are
	// standard SQL, the same idiom bucketedIDExpr uses for the rollup
	// dimensions.
	return fmt.Sprintf(`COALESCE(NULLIF(%s, ''), '%s')`, extract, unassignedDimensionID), true
}

// metadataDimensionSource resolves a metadata dimension to the same
// dimensionReadSource the column-backed dimensions use - no fan-out (one key
// holds at most one value per row), no matview path (the hourly view carries
// fixed columns, never a caller-chosen key), no dimension ceiling (a metadata
// value is a label on the request, not an organisation id, so DimensionScope
// has nothing to bind to it - the same treatment provider/alias already get).
// The expression already buckets the unattributed, so the source reads the
// logs table directly.
func metadataDimensionSource(dialect, key string) (dimensionReadSource, bool) {
	expr, ok := metadataGroupExpr(dialect, key)
	if !ok {
		return dimensionReadSource{}, false
	}
	// id and name are the same expression: the extracted value is already the
	// human-readable label, exactly like the app and user_agent dimensions.
	// Bucketed stays false deliberately - that flag wires the organisation
	// fan-out and hidden-entity totals in the column-backed path, none of
	// which applies here; the unassigned collapse lives in the expression
	// itself instead.
	return dimensionReadSource{IDExpr: expr, NameCol: expr}, true
}

// topDimensionValues picks the k dimension values with the highest total
// metric across the rows of one histogram window. rows is a row count, valueOf
// and metricOf project the value and primary metric (cost, tokens, requests -
// whichever the histogram ranks by) out of row i. Every value is kept when the
// distinct count fits in k, so the common low-cardinality case merges nothing.
func topDimensionValues(rows int, valueOf func(i int) string, metricOf func(i int) float64, k int) map[string]bool {
	totals := make(map[string]float64)
	for i := 0; i < rows; i++ {
		totals[valueOf(i)] += metricOf(i)
	}
	if len(totals) <= k {
		keep := make(map[string]bool, len(totals))
		for v := range totals {
			keep[v] = true
		}
		return keep
	}
	// Sorting keeps the pick deterministic for ties at the boundary: total
	// metric descending, the value itself ascending - so the result never
	// depends on map iteration order.
	values := make([]string, 0, len(totals))
	for v := range totals {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool {
		mi, mj := totals[values[i]], totals[values[j]]
		if mi != mj {
			return mi > mj
		}
		return values[i] < values[j]
	})
	keep := make(map[string]bool, k)
	for _, v := range values[:k] {
		keep[v] = true
	}
	return keep
}

// GetMetadataDimensionRankings ranks one caller-chosen metadata key the same
// way GetDimensionRankings ranks a column-backed dimension: grouped, ordered
// by request count, limited, with a trend against the immediately preceding
// period. It is its own function rather than a branch threaded through
// GetDimensionRankings for the same reason the JSON-field dimensions are:
// there is no materialized-view fast path (the matviews carry fixed columns,
// never a caller-chosen key), and the attribution rules differ - here every
// terminal row lands in exactly one group because the grouping expression
// itself collapses absent and non-string values into unassigned, so
// TotalAttributedRequests always equals TotalActualRequests and one count
// serves both.
//
// No dimension ceiling applies: a metadata value is a caller-written label on
// the request, not an organisation id, so DimensionScope has nothing to bind
// to it - row visibility is still enforced by scopedLogsDB, exactly as it is
// for the provider/alias dimensions.
func (s *RDBLogStore) GetMetadataDimensionRankings(ctx context.Context, filters SearchFilters, dimension RankingDimension, key string) (*DimensionRankingResult, error) {
	src, ok := metadataDimensionSource(s.db.Dialector.Name(), key)
	if !ok {
		return nil, fmt.Errorf("invalid ranking dimension: %s", dimension)
	}
	groupExpr := src.IDExpr

	selectClause := fmt.Sprintf(`
		%s as id,
		MAX(%s) as name,
		COUNT(*) as total_requests,
		SUM(total_tokens) as total_tokens,
		COALESCE(SUM(cost), 0) as total_cost
	`, groupExpr, src.NameCol)

	currentQuery := src.base(s.scopedLogsDB(ctx))
	currentQuery = s.applyFilters(currentQuery, filters)
	currentQuery = currentQuery.Where("status IN ?", terminalLogStatuses)

	var currentResults []dimensionRankingRow

	if err := applyRankingLimit(currentQuery.
		Select(selectClause).
		Group(groupExpr).
		// Tiebreak on the group expression itself, matching
		// GetDimensionRankings: ClickHouse resolves a bare `id` in ORDER BY
		// to the base relation's column rather than the SELECT alias.
		Order("total_requests DESC, "+groupExpr+" ASC"), filters).
		Find(&currentResults).Error; err != nil {
		return nil, fmt.Errorf("failed to get dimension rankings for %s: %w", dimension, err)
	}

	// Every terminal row in the window is counted by exactly one group -
	// unattributed rows included - so the real request count and the sum of
	// every ranking row are the same number, read once off the base table.
	var total int64
	countQuery := s.scopedLogsDB(ctx).Model(&Log{})
	countQuery = s.applyFilters(countQuery, filters)
	countQuery = countQuery.Where("status IN ?", terminalLogStatuses)
	if err := countQuery.Count(&total).Error; err != nil {
		return nil, fmt.Errorf("failed to get dimension ranking totals for %s: %w", dimension, err)
	}

	if len(currentResults) == 0 {
		return &DimensionRankingResult{
			Rankings:                []DimensionRankingWithTrend{},
			Dimension:               dimension,
			TotalActualRequests:     total,
			TotalAttributedRequests: total,
		}, nil
	}

	prevMap := make(map[string]DimensionRankingEntry)
	if filters.StartTime != nil && filters.EndTime != nil {
		duration := filters.EndTime.Sub(*filters.StartTime)
		prevStart := filters.StartTime.Add(-duration)
		prevEnd := filters.StartTime.Add(-time.Nanosecond)

		prevFilters := filters
		prevFilters.StartTime = &prevStart
		prevFilters.EndTime = &prevEnd

		prevQuery := src.base(s.scopedLogsDB(ctx))
		prevQuery = s.applyFilters(prevQuery, prevFilters)
		prevQuery = prevQuery.Where("status IN ?", terminalLogStatuses)

		ids := make([]string, len(currentResults))
		for i, r := range currentResults {
			ids[i] = r.ID
		}
		// One bind for the whole list: in export mode (no ranking limit) the
		// list can exceed the per-statement parameter limit.
		prevQuery = prevQuery.Where(queryscope.InStrings(prevQuery, groupExpr, ids))

		var prevResults []struct {
			ID            string          `gorm:"column:id"`
			TotalRequests int64           `gorm:"column:total_requests"`
			TotalTokens   sql.NullInt64   `gorm:"column:total_tokens"`
			TotalCost     sql.NullFloat64 `gorm:"column:total_cost"`
		}
		prevSelect := fmt.Sprintf(`
			%s as id,
			COUNT(*) as total_requests,
			SUM(total_tokens) as total_tokens,
			COALESCE(SUM(cost), 0) as total_cost
		`, groupExpr)
		if err := prevQuery.
			Select(prevSelect).
			Group(groupExpr).
			Find(&prevResults).Error; err != nil {
			return nil, fmt.Errorf("failed to get previous period dimension rankings for %s: %w", dimension, err)
		}
		for _, r := range prevResults {
			prevMap[r.ID] = DimensionRankingEntry{
				ID:            r.ID,
				TotalRequests: r.TotalRequests,
				TotalTokens:   r.TotalTokens.Int64,
				TotalCost:     r.TotalCost.Float64,
			}
		}
	}

	rankings := make([]DimensionRankingWithTrend, len(currentResults))
	for i, r := range currentResults {
		name := r.Name
		// The grouping expression labels the unattributed with the bare id;
		// give the bucket the same display label the rollup dimensions use.
		if r.ID == unassignedDimensionID {
			name = unassignedDimensionName
		}
		entry := DimensionRankingEntry{
			ID:            r.ID,
			Name:          name,
			TotalRequests: r.TotalRequests,
			TotalTokens:   r.TotalTokens.Int64,
			TotalCost:     r.TotalCost.Float64,
		}
		var trend DimensionRankingTrend
		if prev, exists := prevMap[r.ID]; exists && prev.TotalRequests > 0 {
			trend.HasPreviousPeriod = true
			trend.RequestsTrend = pctChange(float64(prev.TotalRequests), float64(r.TotalRequests))
			trend.TokensTrend = metricTrend(float64(prev.TotalTokens), float64(r.TotalTokens.Int64))
			trend.CostTrend = metricTrend(prev.TotalCost, r.TotalCost.Float64)
		}
		rankings[i] = DimensionRankingWithTrend{DimensionRankingEntry: entry, Trend: trend}
	}

	return &DimensionRankingResult{
		Rankings:                rankings,
		Dimension:               dimension,
		TotalActualRequests:     total,
		TotalAttributedRequests: total,
	}, nil
}

// histogramDimensionRead is how one dimension histogram reads its series: the
// relation and expressions to group on, plus what the post-processing needs.
type histogramDimensionRead struct {
	// src is the read source; for metadata dimensions it is the plain logs
	// table with the bucketing extraction as the id expression.
	src dimensionReadSource
	// dimValueExpr yields the series value in SELECT.
	dimValueExpr string
	// groupCol is the GROUP BY expression.
	groupCol string
	// dimCol is the organisation column the dimension ceiling binds, or ""
	// for metadata dimensions - a metadata value is a label on the request,
	// not an organisation id, so no ceiling applies (row visibility is still
	// enforced by scopedLogsDB).
	dimCol string
	// metadataKey is the grouped metadata key, or "" for column-backed
	// dimensions. Non-empty means: no matview path exists (the hourly view
	// carries fixed columns only), and series outside the window's top
	// maxMetadataDimensionHistogramValues merge into "other" after the query.
	metadataKey string
}

// histogramDimensionReadFor resolves how a dimension histogram reads one
// dimension, unifying what the cost/token/latency histograms previously
// spelled out in three identical prologues. The compound metadata_<key> form
// resolves through the metadata extraction; everything else is the
// column-backed resolution unchanged.
func (s *RDBLogStore) histogramDimensionReadFor(dimension HistogramDimension, dialect string) (histogramDimensionRead, error) {
	if key, isMetadata := ParseMetadataDimension(string(dimension)); isMetadata {
		src, ok := metadataDimensionSource(dialect, key)
		if !ok {
			return histogramDimensionRead{}, fmt.Errorf("invalid histogram dimension: %s", dimension)
		}
		return histogramDimensionRead{
			src:          src,
			dimValueExpr: src.IDExpr,
			groupCol:     src.IDExpr,
			metadataKey:  key,
		}, nil
	}
	dimCol, ok := histogramDimensionColumn(dimension)
	if !ok {
		return histogramDimensionRead{}, fmt.Errorf("invalid histogram dimension: %s", dimension)
	}
	// Rollup dimensions bucket owner-less traffic as "unassigned"; team /
	// customer / business unit additionally fan out to every id the row
	// carries. Both force the raw path - the matview reader has neither the
	// unassigned bucket nor the array columns.
	src := s.dimensionReadSourceFor(dimCol, "")
	dimValueExpr := src.IDExpr
	groupCol := dimValueExpr
	if !src.Bucketed {
		dimValueExpr = fmt.Sprintf("COALESCE(%s, '')", dimCol)
		groupCol = dimCol
	}
	return histogramDimensionRead{
		src:          src,
		dimValueExpr: dimValueExpr,
		groupCol:     groupCol,
		dimCol:       dimCol,
	}, nil
}

// IsValidHistogramDimension reports whether a dimension the histogram
// endpoints can serve: one of the fixed ValidHistogramDimensions, or the
// compound metadata_<key> form naming a well-formed caller metadata key.
func IsValidHistogramDimension(dim HistogramDimension) bool {
	if ValidHistogramDimensions[dim] {
		return true
	}
	_, ok := ParseMetadataDimension(string(dim))
	return ok
}

// IsValidRankingDimension reports what ValidRankingDimensions reports, plus
// the compound metadata_<key> form. The fixed set stays the public contract
// for the column-backed dimensions; metadata keys cannot be enumerated ahead
// of time, so they are validated by shape instead.
func IsValidRankingDimension(dim RankingDimension) bool {
	if ValidRankingDimensions[dim] {
		return true
	}
	_, ok := ParseMetadataDimension(string(dim))
	return ok
}
