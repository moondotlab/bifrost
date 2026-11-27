package logstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// metadataLog builds one terminal log row carrying the given raw metadata JSON
// ("" or nil for no metadata at all). The Metadata field is set directly rather
// than through MetadataParsed, so the exact bytes reach the database on every
// backend - the same guarantee the malformed-row fixtures in
// jsonfielddimensions_test.go rely on.
func metadataLog(id string, ts time.Time, metadataJSON string, cost float64, tokens int) *Log {
	entry := &Log{
		ID: id, Timestamp: ts, Object: "chat_completion", Provider: "openai", Model: "gpt-4o",
		Status: "success", Cost: new(cost), TotalTokens: tokens, Latency: new(100.0), CreatedAt: ts,
	}
	if metadataJSON != "" {
		entry.Metadata = &metadataJSON
	}
	return entry
}

// seedMetadataDimensionFixtures writes the attribution matrix every assertion
// below works off:
//   - two "user" rows distinguishable by bot_id, one "routine" row - the
//     attributed traffic.
//   - five rows that must ALL collapse into unassigned, each for a different
//     reason: the key absent, a non-string value, an empty-string value,
//     malformed metadata JSON (which must not fail the query), and no metadata
//     column at all.
//   - one row in the previous window, so trend math has something to bite on.
func seedMetadataDimensionFixtures(t *testing.T, ctx context.Context, store LogStore, base time.Time) {
	t.Helper()
	rows := []*Log{
		metadataLog("md-user-alpha", base, `{"origin":"user","bot_id":"bot_alpha"}`, 0.01, 10),
		metadataLog("md-user-beta", base.Add(time.Second), `{"origin":"user","bot_id":"bot_beta"}`, 0.02, 20),
		metadataLog("md-routine", base.Add(2*time.Second), `{"origin":"routine"}`, 0.04, 40),
		metadataLog("md-no-key", base.Add(3*time.Second), `{"other":"x"}`, 0.08, 5),
		metadataLog("md-nonstring", base.Add(4*time.Second), `{"origin":42}`, 0.10, 6),
		metadataLog("md-empty", base.Add(5*time.Second), `{"origin":""}`, 0.20, 7),
		metadataLog("md-malformed", base.Add(6*time.Second), `{not valid json`, 0.40, 8),
		metadataLog("md-no-metadata", base.Add(7*time.Second), ``, 0.80, 9),
		// Previous window (the window is base±1h, so the previous one ends at
		// base-1h): one more "user" row, giving the user group a trend.
		metadataLog("md-prev-user", base.Add(-90*time.Minute), `{"origin":"user","bot_id":"bot_alpha"}`, 0.01, 10),
	}
	for _, row := range rows {
		require.NoError(t, store.Create(ctx, row), "insert %s", row.ID)
	}
}

func metadataDimensionWindow(base time.Time) SearchFilters {
	start := base.Add(-time.Hour)
	end := base.Add(time.Hour)
	return SearchFilters{StartTime: &start, EndTime: &end}
}

func assertMetadataDimensionRankings(t *testing.T, ctx context.Context, store LogStore, base time.Time) {
	t.Helper()
	filters := metadataDimensionWindow(base)

	t.Run("groups string values and collapses every flavor of absent into unassigned", func(t *testing.T) {
		res, err := store.GetDimensionRankings(ctx, filters, RankingDimension("metadata_origin"))
		require.NoError(t, err)
		byID := rankingByID(res)

		require.Contains(t, byID, "user")
		assert.Equal(t, int64(2), byID["user"].TotalRequests)
		assert.InDelta(t, 0.03, byID["user"].TotalCost, 1e-9)
		assert.Equal(t, int64(30), byID["user"].TotalTokens)

		require.Contains(t, byID, "routine")
		assert.Equal(t, int64(1), byID["routine"].TotalRequests)
		assert.InDelta(t, 0.04, byID["routine"].TotalCost, 1e-9)

		// no-key + nonstring + empty + malformed + no-metadata = 5 rows,
		// costs 0.08+0.10+0.20+0.40+0.80. The malformed row proves the query
		// does not fail on bad JSON; the non-string row proves numbers are
		// absent, not groups.
		require.Contains(t, byID, unassignedDimensionID)
		assert.Equal(t, int64(5), byID[unassignedDimensionID].TotalRequests)
		assert.InDelta(t, 1.58, byID[unassignedDimensionID].TotalCost, 1e-9)
		assert.Equal(t, unassignedDimensionName, byID[unassignedDimensionID].Name)

		assert.NotContains(t, byID, "42", "the numeric value must not form a group")
		assert.NotContains(t, byID, "", "the empty string must not form a group")

		// The groups are an additive split of the window: 8 terminal rows.
		assert.Equal(t, int64(8), res.TotalActualRequests)
		assert.Equal(t, int64(8), res.TotalAttributedRequests)
		var sum int64
		for _, r := range res.Rankings {
			sum += r.TotalRequests
		}
		assert.Equal(t, int64(8), sum, "groups must reconcile to the window's traffic")

		// Ordered by count descending: unassigned (5) first, then user (2),
		// then routine (1).
		require.Len(t, res.Rankings, 3)
		assert.Equal(t, unassignedDimensionID, res.Rankings[0].ID)
		assert.Equal(t, "user", res.Rankings[1].ID)
		assert.Equal(t, "routine", res.Rankings[2].ID)

		// The compound dimension echoes back so clients can key their cache.
		assert.Equal(t, RankingDimension("metadata_origin"), res.Dimension)
	})

	t.Run("trend compares against the immediately preceding window", func(t *testing.T) {
		res, err := store.GetDimensionRankings(ctx, filters, RankingDimension("metadata_origin"))
		require.NoError(t, err)
		byID := rankingByID(res)
		user := byID["user"]
		assert.True(t, user.Trend.HasPreviousPeriod, "one user row exists in the previous window")
		assert.InDelta(t, 100.0, user.Trend.RequestsTrend, 1e-9) // 1 -> 2 requests
	})

	t.Run("a second independent key groups the same rows differently", func(t *testing.T) {
		res, err := store.GetDimensionRankings(ctx, filters, RankingDimension("metadata_bot_id"))
		require.NoError(t, err)
		byID := rankingByID(res)
		assert.Contains(t, byID, "bot_alpha")
		assert.Contains(t, byID, "bot_beta")
		assert.Equal(t, int64(6), byID[unassignedDimensionID].TotalRequests,
			"everything but the two bot rows is unassigned on this key")
	})

	t.Run("metadata filters combine with metadata grouping", func(t *testing.T) {
		filtered := filters
		filtered.MetadataFilters = MetadataFilterMap{"bot_id": {"bot_alpha"}}
		res, err := store.GetDimensionRankings(ctx, filtered, RankingDimension("metadata_origin"))
		require.NoError(t, err)
		byID := rankingByID(res)
		require.Len(t, res.Rankings, 1, "only md-user-alpha matches the filter")
		assert.Equal(t, int64(1), byID["user"].TotalRequests)
		assert.Equal(t, int64(1), res.TotalActualRequests)
	})

	t.Run("keys with dots and hyphens group literally", func(t *testing.T) {
		dotted := metadataLog("md-dotted", base.Add(8*time.Second), `{"app.env":"prod","x-key":"v1"}`, 0.01, 1)
		require.NoError(t, store.Create(ctx, dotted))
		res, err := store.GetDimensionRankings(ctx, filters, RankingDimension("metadata_app.env"))
		require.NoError(t, err)
		byID := rankingByID(res)
		require.Contains(t, byID, "prod")
		assert.Equal(t, int64(1), byID["prod"].TotalRequests)
		res2, err := store.GetDimensionRankings(ctx, filters, RankingDimension("metadata_x-key"))
		require.NoError(t, err)
		assert.Contains(t, rankingByID(res2), "v1")
	})
}

// TestMetadataDimensionRankings_SQLite runs the assertion suite against
// SQLite alone, so the extraction logic gets real coverage from a plain
// `go test` without the docker-compose Postgres/ClickHouse services - the
// same contract TestJSONFieldDimensionRankings_SQLite establishes.
func TestMetadataDimensionRankings_SQLite(t *testing.T) {
	store, _ := newFanoutTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	seedMetadataDimensionFixtures(t, ctx, store, base)
	assertMetadataDimensionRankings(t, ctx, store, base)
}

// TestMetadataDimensionRankings_ThreeWayParity runs the same suite against
// every reachable backend, proving the three hand-written dialect extraction
// expressions agree with each other rather than merely looking plausible -
// the same contract as TestJSONFieldDimensionRankings_ThreeWayParity.
func TestMetadataDimensionRankings_ThreeWayParity(t *testing.T) {
	ch := trySetupClickHouseStore(t)
	pgDB := trySetupPostgresDB(t)
	var pg *RDBLogStore
	if pgDB != nil {
		dropAllManagedMatViews(pgDB)
		require.NoError(t, pgDB.Exec("DROP TABLE IF EXISTS mcp_tool_logs CASCADE").Error)
		require.NoError(t, pgDB.Exec("DROP TABLE IF EXISTS async_jobs CASCADE").Error)
		require.NoError(t, pgDB.Exec("DROP TABLE IF EXISTS webhook_deliveries CASCADE").Error)
		require.NoError(t, pgDB.Exec("DROP TABLE IF EXISTS logs CASCADE").Error)
		require.NoError(t, pgDB.Exec("CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)").Error)
		require.NoError(t, pgDB.Exec("DELETE FROM migrations").Error)
		require.NoError(t, triggerMigrations(context.Background(), pgDB, testLogger{}))
		pg = &RDBLogStore{db: pgDB, logger: testLogger{}}
	}

	sqliteStore, _ := newFanoutTestStore(t)

	backends := map[string]LogStore{"sqlite": sqliteStore}
	if pg != nil {
		backends[parityReference] = pg
	} else {
		t.Log("Postgres not available, skipping its leg of the parity check")
	}
	if ch != nil {
		backends["clickhouse"] = ch
	}

	ctx := context.Background()
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	for name, store := range backends {
		t.Run(name, func(t *testing.T) {
			seedMetadataDimensionFixtures(t, ctx, store, base)
			// Histograms first: the rankings suite's last subtest inserts an
			// extra dotted-key row, which must not pollute the histograms'
			// exact unassigned totals.
			assertMetadataDimensionHistograms(t, ctx, store, base)
			assertMetadataDimensionRankings(t, ctx, store, base)
		})
	}
}

// assertMetadataDimensionHistograms checks the histogram side: correct series
// per bucket, the unassigned series, and cost conservation.
func assertMetadataDimensionHistograms(t *testing.T, ctx context.Context, store LogStore, base time.Time) {
	t.Helper()
	filters := metadataDimensionWindow(base)

	t.Run("cost histogram groups series by metadata value", func(t *testing.T) {
		res, err := store.GetDimensionCostHistogram(ctx, filters, 3600, HistogramDimension("metadata_origin"))
		require.NoError(t, err)
		require.NotEmpty(t, res.Buckets)
		// All fixture rows sit in the hour bucket containing base (12:00).
		var bucket *DimensionCostHistogramBucket
		for i := range res.Buckets {
			if res.Buckets[i].Timestamp.Equal(base.Truncate(time.Hour)) || res.Buckets[i].Timestamp.Equal(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)) {
				bucket = &res.Buckets[i]
			}
		}
		require.NotNil(t, bucket, "the bucket holding the fixtures exists")
		assert.InDelta(t, 0.03, bucket.ByDimension["user"], 1e-9)
		assert.InDelta(t, 0.04, bucket.ByDimension["routine"], 1e-9)
		assert.InDelta(t, 1.58, bucket.ByDimension[unassignedDimensionID], 1e-9)
		// Conservation: the split still sums to the bucket total.
		var split float64
		for _, v := range bucket.ByDimension {
			split += v
		}
		assert.InDelta(t, bucket.TotalCost, split, 1e-9)
		assert.Contains(t, res.DimensionValues, "user")
		assert.Contains(t, res.DimensionValues, unassignedDimensionID)
	})

	t.Run("token histogram groups series by metadata value", func(t *testing.T) {
		res, err := store.GetDimensionTokenHistogram(ctx, filters, 3600, HistogramDimension("metadata_origin"))
		require.NoError(t, err)
		require.NotEmpty(t, res.Buckets)
		var found bool
		for _, b := range res.Buckets {
			if stats, ok := b.ByDimension["user"]; ok {
				assert.Equal(t, int64(30), stats.TotalTokens)
				found = true
			}
		}
		assert.True(t, found, "user series present in some bucket")
	})

	t.Run("latency histogram groups series by metadata value", func(t *testing.T) {
		res, err := store.GetDimensionLatencyHistogram(ctx, filters, 3600, HistogramDimension("metadata_origin"))
		require.NoError(t, err)
		require.NotEmpty(t, res.Buckets)
		var found bool
		for _, b := range res.Buckets {
			if stats, ok := b.ByDimension["user"]; ok {
				assert.Equal(t, int64(2), stats.TotalRequests)
				assert.InDelta(t, 100.0, stats.AvgLatency, 1e-9)
				found = true
			}
		}
		assert.True(t, found, "user series present in some bucket")
	})
}

func TestMetadataDimensionHistograms_SQLite(t *testing.T) {
	store, _ := newFanoutTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	seedMetadataDimensionFixtures(t, ctx, store, base)
	assertMetadataDimensionHistograms(t, ctx, store, base)
}

// TestMetadataHistogramTopKCollapse seeds more distinct values than the cap
// and asserts the tail merges into "other" while the totals are conserved.
func TestMetadataHistogramTopKCollapse(t *testing.T) {
	store, _ := newFanoutTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	// 30 distinct values with strictly increasing cost, so the top-25 pick is
	// deterministic: v05..v29 stay, v00..v04 merge.
	for i := 0; i < 30; i++ {
		value := fmt.Sprintf("v%02d", i)
		row := metadataLog(fmt.Sprintf("md-card-%02d", i), base.Add(time.Duration(i)*time.Second),
			fmt.Sprintf(`{"origin":"%s"}`, value), float64(i+1)*0.001, i+1)
		require.NoError(t, store.Create(ctx, row))
	}

	filters := metadataDimensionWindow(base)
	res, err := store.GetDimensionCostHistogram(ctx, filters, 3600, HistogramDimension("metadata_origin"))
	require.NoError(t, err)

	assert.Contains(t, res.DimensionValues, metadataDimensionOtherValue)
	assert.NotContains(t, res.DimensionValues, "v00", "the cheapest value merges into other")
	assert.NotContains(t, res.DimensionValues, "v04")
	assert.Contains(t, res.DimensionValues, "v05", "the 25th most expensive value stays")
	assert.Contains(t, res.DimensionValues, "v29")
	// 25 top values + other; no unassigned row exists in this fixture.
	assert.Len(t, res.DimensionValues, maxMetadataDimensionHistogramValues+1)

	// Conservation: merging moves cost, it never drops it - every bucket's
	// split still sums to its total.
	var totalCost float64
	for i := 0; i < 30; i++ {
		totalCost += float64(i+1) * 0.001
	}
	var gotTotal float64
	for _, b := range res.Buckets {
		gotTotal += b.TotalCost
		var split float64
		for _, v := range b.ByDimension {
			split += v
		}
		assert.InDelta(t, b.TotalCost, split, 1e-9, "bucket %d split reconciles", b.Timestamp.Unix())
	}
	assert.InDelta(t, totalCost, gotTotal, 1e-9)

	// The merged series carries exactly the cost of v00..v04.
	var otherCost float64
	for _, b := range res.Buckets {
		otherCost += b.ByDimension[metadataDimensionOtherValue]
	}
	assert.InDelta(t, (0.001 + 0.002 + 0.003 + 0.004 + 0.005), otherCost, 1e-9)
}

// TestParseMetadataDimension covers the validation boundary: everything that
// reaches SQL splicing passes through here, so the charset rejection IS the
// injection defense.
func TestParseMetadataDimension(t *testing.T) {
	valid := map[string]string{
		"metadata_origin":   "origin",
		"metadata_bot_id":   "bot_id",
		"metadata_app.env":  "app.env",
		"metadata_x-key":    "x-key",
		"metadata_a.b-c_d9": "a.b-c_d9",
	}
	for dim, want := range valid {
		key, ok := ParseMetadataDimension(dim)
		assert.True(t, ok, "%s should parse", dim)
		assert.Equal(t, want, key)
	}

	invalid := []string{
		"",                       // empty
		"origin",                 // no prefix
		"metadata_",              // empty key
		"metadata_a'b",           // quote - SQL injection vector
		"metadata_a\"b",          // double quote
		"metadata_a b",           // space
		"metadata_a;b",           // semicolon
		"metadata_a(b)",          // parens
		"metadata_isAsyncRequest", // system key
		"metadata_bifrost_alb_decision", // load-balancer system prefix
		"provider",               // a column dimension, not the compound form
		"metadata_" + string(make([]byte, 257)), // too long (257 chars, non-charset too)
	}
	for _, dim := range invalid {
		_, ok := ParseMetadataDimension(dim)
		assert.False(t, ok, "%q must not parse", dim)
	}

	// A maximal-length valid key (256 chars) passes.
	longKey := make([]byte, 256)
	for i := range longKey {
		longKey[i] = 'a'
	}
	_, ok := ParseMetadataDimension("metadata_" + string(longKey))
	assert.True(t, ok, "256-char key is within the limit")
}

// TestIsValidDimensionHelpers pins the handler-facing validation: fixed
// dimensions keep working, the compound form validates by shape.
func TestIsValidDimensionHelpers(t *testing.T) {
	assert.True(t, IsValidHistogramDimension(DimensionProvider))
	assert.True(t, IsValidHistogramDimension(HistogramDimension("metadata_origin")))
	assert.False(t, IsValidHistogramDimension(HistogramDimension("metadata_")))
	assert.False(t, IsValidHistogramDimension(HistogramDimension("nonsense")))

	assert.True(t, IsValidRankingDimension(RankingDimensionUserAgent))
	assert.True(t, IsValidRankingDimension(RankingDimension("metadata_origin")))
	assert.False(t, IsValidRankingDimension(RankingDimension("metadata_isAsyncRequest")))
	assert.False(t, IsValidRankingDimension(RankingDimension("nonsense")))
}

// BenchmarkMetadataDimensionRankings measures what the JSON extraction costs
// next to the column-backed equivalent over the same rows: same window, same
// terminals, one grouping per query. It answers the cost question this
// feature ships with - the extraction is per-row CPU the column dimensions
// never pay, and the ratio is what matters, not the absolute number (which is
// sqlite on one machine).
func BenchmarkMetadataDimensionRankings(b *testing.B) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		b.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&Log{}); err != nil {
		b.Fatal(err)
	}
	store := &RDBLogStore{db: db}

	// 2000 terminal rows, each with a realistic metadata blob (a few keys, as
	// x-bf-lh-* headers write them) and a user_agent so the column-backed
	// baseline groups the same population into the same number of groups.
	ctx := context.Background()
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	rows := make([]*Log, 0, 2000)
	for i := 0; i < 2000; i++ {
		origin := fmt.Sprintf("origin-%d", i%10)
		metadata := fmt.Sprintf(`{"origin":"%s","bot_id":"bot_%d","env":"production","region":"us-east-1"}`, origin, i%25)
		rows = append(rows, &Log{
			ID: fmt.Sprintf("bench-%d", i), Timestamp: base.Add(time.Duration(i) * time.Second),
			Object: "chat_completion", Provider: "openai", Model: "gpt-4o",
			Status: "success", Cost: new(0.01), TotalTokens: 10,
			UserAgent: &origin, Metadata: &metadata, CreatedAt: base,
		})
	}
	if err := store.BatchCreateIfNotExists(ctx, rows); err != nil {
		b.Fatal(err)
	}
	start := base.Add(-time.Hour)
	end := base.Add(3 * time.Hour)
	filters := SearchFilters{StartTime: &start, EndTime: &end}

	b.Run("metadata_key", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := store.GetDimensionRankings(ctx, filters, RankingDimension("metadata_origin")); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("column_baseline", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := store.GetDimensionRankings(ctx, filters, RankingDimensionUserAgent); err != nil {
				b.Fatal(err)
			}
		}
	})
}
