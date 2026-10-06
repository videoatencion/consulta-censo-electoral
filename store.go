// vim: set ts=4 sw=4 noet:
package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// schemaVersion is stored in PRAGMA user_version. Bump it whenever the schema
// changes so a database built by an older release is not served by mistake.
const schemaVersion = 2

// maxMatches bounds how many rows a single lookup can return.
const maxMatches = 500

const schema = `
	CREATE TABLE polling_stations (
		id INTEGER PRIMARY KEY,
		poblacion TEXT NOT NULL,
		colele TEXT NOT NULL,
		dircol TEXT NOT NULL,
		UNIQUE (poblacion, colele, dircol)
	);
	CREATE TABLE citizens (
		citizen_id TEXT NOT NULL,
		day TEXT NOT NULL,
		year TEXT NOT NULL,
		fn TEXT NOT NULL,
		sn1 TEXT NOT NULL,
		sn2 TEXT NOT NULL,
		postCode TEXT NOT NULL,
		station_id INTEGER NOT NULL REFERENCES polling_stations (id),
		distrito TEXT NOT NULL,
		seccion TEXT NOT NULL,
		mesa TEXT NOT NULL,
		PRIMARY KEY (citizen_id, day, year, fn, sn1, sn2, postCode)
	) WITHOUT ROWID;
`

type CitizenInfo struct {
	Poblacion    string `json:"poblacion"`
	Distrito     string `json:"distrito"`
	Seccion      string `json:"seccion"`
	Mesa         string `json:"mesa"`
	Colele       string `json:"colele"`
	Dircol       string `json:"dircol"`
	PostCode     string `json:"postCode"`
	ErrorMessage string `json:"errorMessage"`
}

type CitizenKey struct {
	CitizenID string
	Day       string
	Year      string
	Fn        string
	Sn1       string
	Sn2       string
	PostCode  string
	Colele    string
}

// keyFields lists the optional key columns from the easiest to the hardest
// for a citizen to answer. It breaks ties when ranking the fields to ask for.
var keyFields = []struct {
	name, column string
	get          func(CitizenKey) string
}{
	{"day", "day", func(k CitizenKey) string { return k.Day }},
	{"year", "year", func(k CitizenKey) string { return k.Year }},
	{"sn1", "sn1", func(k CitizenKey) string { return k.Sn1 }},
	{"sn2", "sn2", func(k CitizenKey) string { return k.Sn2 }},
	{"fn", "fn", func(k CitizenKey) string { return k.Fn }},
	{"postCode", "postCode", func(k CitizenKey) string { return k.PostCode }},
}

func sqliteDSN(path string, readOnly bool, pragmas ...string) string {
	q := url.Values{}
	if readOnly {
		q.Set("mode", "ro")
	}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	return "file:" + path + "?" + q.Encode()
}

// openStore opens an already built database for serving.
func openStore(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path, true, "busy_timeout(5000)", "query_only(1)"))
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("reading database version: %w", err)
	}
	if version != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("%s was built by another release (schema %d, expected %d): place the census CSV in the data directory to rebuild it", path, version, schemaVersion)
	}
	return db, nil
}

// lookupCitizen returns the polling station for key. When the key matches
// citizens voting in different places it returns, as the error, the list of
// fields that would tell them apart (e.g. "[day sn1]").
func lookupCitizen(db *sql.DB, key CitizenKey) (CitizenInfo, error) {
	query := `
		SELECT
			c.day, c.year, c.fn, c.sn1, c.sn2, c.postCode, c.distrito, c.seccion, c.mesa,
			p.poblacion, p.colele, p.dircol
		FROM
			citizens c
			JOIN polling_stations p ON p.id = c.station_id
		WHERE
			c.citizen_id = ?`
	args := []any{key.CitizenID}

	for _, f := range keyFields {
		if v := f.get(key); v != "" {
			query += " AND c." + f.column + " = ?"
			args = append(args, v)
		}
	}
	if key.Colele != "" {
		query += " AND upper(p.colele) = ?"
		args = append(args, key.Colele)
	}
	query += fmt.Sprintf(" LIMIT %d", maxMatches)

	rows, err := db.Query(query, args...)
	if err != nil {
		return CitizenInfo{}, err
	}
	defer rows.Close()

	var keys []CitizenKey
	var infos []CitizenInfo
	for rows.Next() {
		var k CitizenKey
		var info CitizenInfo
		err := rows.Scan(&k.Day, &k.Year, &k.Fn, &k.Sn1, &k.Sn2, &k.PostCode,
			&info.Distrito, &info.Seccion, &info.Mesa,
			&info.Poblacion, &info.Colele, &info.Dircol)
		if err != nil {
			return CitizenInfo{}, err
		}
		k.Colele = info.Colele
		info.PostCode = k.PostCode
		keys = append(keys, k)
		infos = append(infos, info)
	}
	if err := rows.Err(); err != nil {
		return CitizenInfo{}, err
	}

	if len(infos) == 0 {
		return CitizenInfo{}, fmt.Errorf("no records found")
	}

	// Several citizens may share the key and still vote at the same table;
	// the answer is the same for all of them, so there is no need to ask more.
	same := true
	for _, info := range infos[1:] {
		if !samePlace(info, infos[0]) {
			same = false
			break
		}
	}
	if same {
		result := infos[0]
		if len(infos) > 1 {
			result.PostCode = ""
		}
		return result, nil
	}

	return CitizenInfo{}, fmt.Errorf("%v", findDifferingFields(keys, infos))
}

func samePlace(a, b CitizenInfo) bool {
	return placeOf(a) == placeOf(b)
}

// findDifferingFields returns the fields worth asking for to find out where
// the citizen votes, best first: fields that settle the polling table on their
// own, then those leaving the fewest tables on average, then the easiest to
// answer. A field that does not split the tables at all is left out; when no
// field helps, only the polling station tells them apart ("colele").
func findDifferingFields(keys []CitizenKey, infos []CitizenInfo) []string {
	type ranked struct {
		name      string
		resolves  bool
		remaining float64 // expected number of tables left after answering
		ease      int
	}

	allPlaces := map[CitizenInfo]bool{}
	for _, info := range infos {
		allPlaces[placeOf(info)] = true
	}

	var candidates []ranked
	for ease, f := range keyFields {
		places := map[string]map[CitizenInfo]bool{}
		counts := map[string]int{}
		for i, k := range keys {
			v := f.get(k)
			if places[v] == nil {
				places[v] = map[CitizenInfo]bool{}
			}
			places[v][placeOf(infos[i])] = true
			counts[v]++
		}

		resolves := true
		var remaining float64
		for v, p := range places {
			if len(p) > 1 {
				resolves = false
			}
			remaining += float64(counts[v]) / float64(len(keys)) * float64(len(p))
		}
		if remaining >= float64(len(allPlaces)) {
			continue
		}
		candidates = append(candidates, ranked{f.name, resolves, remaining, ease})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.resolves != b.resolves {
			return a.resolves
		}
		if a.remaining != b.remaining {
			return a.remaining < b.remaining
		}
		return a.ease < b.ease
	})

	fields := make([]string, 0, len(candidates))
	for _, c := range candidates {
		fields = append(fields, c.name)
	}
	if len(fields) == 0 {
		fields = append(fields, "colele")
	}
	return fields
}

// placeOf keeps only the fields that identify a polling table.
func placeOf(info CitizenInfo) CitizenInfo {
	return CitizenInfo{
		Poblacion: info.Poblacion, Colele: info.Colele, Dircol: info.Dircol,
		Distrito: info.Distrito, Seccion: info.Seccion, Mesa: info.Mesa,
	}
}

type ComboResult struct {
	Combo      []string
	Percentage float64
}

// calculateUniquePercentages reports, for the document alone and for the
// document plus each enabled field, the share of citizens whose polling table
// can be resolved with just those fields.
func calculateUniquePercentages(db *sql.DB, cfg Config) ([]ComboResult, error) {
	enabled := map[string]bool{
		"day": cfg.Day, "year": cfg.Year, "fn": cfg.Fn,
		"sn1": cfg.Sn1, "sn2": cfg.Sn2, "postCode": cfg.PostCode,
	}
	combinations := [][]string{{}}
	var all []string
	for _, f := range keyFields {
		if enabled[f.column] {
			combinations = append(combinations, []string{f.column})
			all = append(all, f.column)
		}
	}
	if len(all) > 1 {
		combinations = append(combinations, all)
	}

	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM citizens").Scan(&total); err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, nil
	}

	results := make([]ComboResult, 0, len(combinations))
	for _, combo := range combinations {
		fields := strings.Join(append([]string{"citizen_id"}, combo...), ", ")
		query := fmt.Sprintf(`
			SELECT COALESCE(SUM(n), 0) FROM (
				SELECT COUNT(*) AS n FROM citizens
				GROUP BY %s
				HAVING COUNT(DISTINCT station_id || '|' || distrito || '|' || seccion || '|' || mesa) = 1
			)`, fields)
		var resolved int
		if err := db.QueryRow(query).Scan(&resolved); err != nil {
			return nil, err
		}
		results = append(results, ComboResult{Combo: combo, Percentage: float64(resolved) / float64(total) * 100})
	}
	return results, nil
}

func printResults(results []ComboResult) {
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Percentage > results[j].Percentage
	})
	for _, result := range results {
		log.Printf("%s = %.2f%%", strings.Join(append([]string{"citizen_id"}, result.Combo...), "+"), result.Percentage)
	}
}
