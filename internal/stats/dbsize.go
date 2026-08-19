package stats

import (
	"context"
	"strconv"
	"strings"
)

// dbSizeQuerier is the subset of db.Client the sampler needs — narrowed to
// keep this package from importing the full client just to call one query,
// and so tests can supply a fake. GlobalStatus was folded in alongside Query
// rather than given its own interface: db.Client satisfies both in practice,
// and the live database performance graphs (SampleSystem, in system.go) are
// gathered by the same Sampler instance that already holds this querier for
// schema sizing, so there is no benefit to a second narrow interface over
// the same underlying connection.
type dbSizeQuerier interface {
	Query(ctx context.Context, sql string) (string, error)
	GlobalStatus(ctx context.Context) (map[string]string, error)
}

// schemaSizesMB queries every database's size in a single round trip rather
// than one query per site — the cost is the same regardless of how many
// sites are on the box, which matters because this runs on a timer for as
// long as a dashboard stays open.
func schemaSizesMB(ctx context.Context, q dbSizeQuerier) (map[string]int64, error) {
	const sql = `SELECT table_schema, SUM(data_length + index_length)
FROM information_schema.tables
GROUP BY table_schema;`
	out, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	return parseSchemaSizes(out), nil
}

// parseSchemaSizes turns the client's tab-separated output into a lookup by
// schema name. Kept separate from the query itself so the parsing can be
// tested against fixed sample output without a database.
func parseSchemaSizes(raw string) map[string]int64 {
	sizes := make(map[string]int64)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 2 {
			continue
		}
		bytes, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			// A schema with zero tables reports NULL for the SUM, which the
			// client renders as "NULL" — that is zero size, not a parse
			// failure worth dropping the whole row for.
			continue
		}
		sizes[fields[0]] = bytes / (1 << 20)
	}
	return sizes
}
