// vim: set ts=4 sw=4 noet:
package main

import (
	"bufio"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// columns are the positions of the INE "Formato Servicio de Información
// (SI)" export, whose layout is fixed.
var columns = map[string]int{
	"LMUN": 2, "DIST": 3, "SECC": 4, "MESA": 5, "NLOCAL": 6,
	"DIRMESA1": 9, "DIRMESA2": 10, "DIRMESA3": 11, "DIRMESA4": 12,
	"NOMBRE": 13, "APE1": 14, "APE2": 15, "FNAC": 25, "IDENT": 27, "CPOSTAM": 28,
}

// prepareDatabase imports any census file found in the data directory, or
// opens the database built by a previous run when there is none.
func prepareDatabase(cfg Config) (*sql.DB, error) {
	csvFiles, dbFiles, err := listDataDir(cfg.DataDir)
	if err != nil {
		return nil, err
	}

	dbPath := filepath.Join(cfg.DataDir, "citizens.db")
	if len(dbFiles) > 0 {
		dbPath = dbFiles[0]
		if len(dbFiles) > 1 {
			log.Printf("Several databases found in %s, using %s", cfg.DataDir, dbPath)
		}
	}

	if len(csvFiles) == 0 {
		if len(dbFiles) == 0 {
			return nil, fmt.Errorf("no database or CSV file found in %s", cfg.DataDir)
		}
		log.Printf("No CSV file found, serving %s", dbPath)
		return openStore(dbPath)
	}

	// Build into a temporary file and swap it in only once it is complete,
	// so a failed import never leaves a half populated database behind.
	tmpPath := filepath.Join(cfg.DataDir, ".citizens.db.tmp")
	os.Remove(tmpPath)

	start := time.Now()
	if err := buildDatabase(cfg, tmpPath, csvFiles); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, dbPath); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("replacing database: %w", err)
	}
	log.Printf("Citizens loaded in %v", time.Since(start))

	// The census holds far more personal data than what is indexed: do not
	// keep it around once the database has been built.
	for _, f := range csvFiles {
		if err := os.Remove(f); err != nil {
			log.Printf("Error removing CSV file %s: %v", f, err)
		}
	}

	return openStore(dbPath)
}

// listDataDir returns the census files and the databases in dir, sorted.
func listDataDir(dir string) (csvFiles, dbFiles []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading data directory: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".csv", ".txt":
			csvFiles = append(csvFiles, filepath.Join(dir, e.Name()))
		case ".db":
			dbFiles = append(dbFiles, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(csvFiles)
	sort.Strings(dbFiles)
	return csvFiles, dbFiles, nil
}

// readCensus calls fn for every data row of an INE census file, with a getter
// for the columns listed in columns. The record is reused between calls.
func readCensus(path string, fn func(line int, field func(name string) string) error) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening file: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(bufio.NewReaderSize(file, 1<<20))
	reader.Comma = ';'
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.ReuseRecord = true

	// Read and discard the header line
	if _, err := reader.Read(); err != nil {
		return fmt.Errorf("reading CSV header: %w", err)
	}

	var record []string
	field := func(name string) string {
		if i := columns[name]; i < len(record) {
			return record[i]
		}
		return ""
	}
	for line := 2; ; line++ {
		record, err = reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading CSV file: %w", err)
		}
		if err := fn(line, field); err != nil {
			return err
		}
	}
}

func buildDatabase(cfg Config, path string, csvFiles []string) error {
	db, err := sql.Open("sqlite", sqliteDSN(path, false, "journal_mode(OFF)", "synchronous(OFF)"))
	if err != nil {
		return fmt.Errorf("creating database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	imp, err := newImporter(cfg, tx)
	if err != nil {
		return err
	}
	defer imp.close()

	for _, f := range csvFiles {
		log.Printf("Detected CSV file %s. Starting to parse...", filepath.Base(f))
		if err := imp.importFile(f); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}

	log.Printf("CSV import process: %d rows read, %d rows imported, %d skipped (empty document or birthdate), %d duplicates with the same polling table",
		imp.rowsRead, imp.rowsImported, imp.rowsSkipped, imp.duplicates)

	if imp.collisions > 0 {
		return fmt.Errorf("%d collisions: citizens with the same key vote at different tables (first at line %d). "+
			"Enable more tie-breaking fields (SN2, POST_CODE, FN, YEAR...)", imp.collisions, imp.firstCollision)
	}
	if imp.rowsImported == 0 {
		return errors.New("no citizens imported")
	}

	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	results, err := calculateUniquePercentages(db, cfg)
	if err != nil {
		return fmt.Errorf("calculating unique percentages: %w", err)
	}
	printResults(results)
	return nil
}

type stationKey struct{ poblacion, colele, dircol string }

type importer struct {
	cfg        Config
	insCitizen *sql.Stmt
	insStation *sql.Stmt
	getCitizen *sql.Stmt
	stations   map[stationKey]int64

	rowsRead, rowsImported, rowsSkipped int
	duplicates, collisions              int
	firstCollision                      int
}

func newImporter(cfg Config, tx *sql.Tx) (*importer, error) {
	imp := &importer{cfg: cfg, stations: map[stationKey]int64{}}
	var err error
	if imp.insCitizen, err = tx.Prepare(`
		INSERT INTO citizens (citizen_id, day, year, fn, sn1, sn2, postCode, station_id, distrito, seccion, mesa)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`); err != nil {
		return nil, fmt.Errorf("preparing citizen insert statement: %w", err)
	}
	if imp.insStation, err = tx.Prepare(`INSERT INTO polling_stations (poblacion, colele, dircol) VALUES (?, ?, ?)`); err != nil {
		return nil, fmt.Errorf("preparing polling station insert statement: %w", err)
	}
	if imp.getCitizen, err = tx.Prepare(`
		SELECT station_id, distrito, seccion, mesa FROM citizens
		WHERE citizen_id = ? AND day = ? AND year = ? AND fn = ? AND sn1 = ? AND sn2 = ? AND postCode = ?`); err != nil {
		return nil, fmt.Errorf("preparing citizen select statement: %w", err)
	}
	return imp, nil
}

func (imp *importer) close() {
	for _, s := range []*sql.Stmt{imp.insCitizen, imp.insStation, imp.getCitizen} {
		if s != nil {
			s.Close()
		}
	}
}

func (imp *importer) importFile(path string) error {
	cfg := imp.cfg
	needBirthdate := cfg.Day || cfg.Year

	return readCensus(path, func(line int, field func(string) string) error {
		imp.rowsRead++

		citizenID := cfg.documentKey(field("IDENT"))
		if citizenID == "" {
			imp.rowsSkipped++
			return nil
		}

		var day, year string
		if needBirthdate {
			d, y, ok := birthdateKeys(field("FNAC"))
			if !ok {
				imp.rowsSkipped++
				return nil
			}
			if cfg.Day {
				day = d
			}
			if cfg.Year {
				year = y
			}
		}

		// Additional indexes stay empty unless enabled in the environment.
		var fn, sn1, sn2, postCode string
		if cfg.Fn {
			fn = cfg.nameKey(decodeField(field("NOMBRE")))
		}
		if cfg.Sn1 {
			sn1 = cfg.nameKey(decodeField(field("APE1")))
		}
		if cfg.Sn2 {
			sn2 = cfg.nameKey(decodeField(field("APE2")))
		}
		if cfg.PostCode {
			postCode = strings.TrimSpace(field("CPOSTAM"))
		}

		place := pollingPlaceOf(field)
		stationID, err := imp.station(place.station)
		if err != nil {
			return err
		}

		res, err := imp.insCitizen.Exec(citizenID, day, year, fn, sn1, sn2, postCode, stationID, place.dist, place.secc, place.mesa)
		if err != nil {
			return fmt.Errorf("line %d: inserting citizen: %w", line, err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			imp.rowsImported++
			return nil
		}

		// Another citizen already has this key. It is harmless if both vote
		// at the same table; otherwise the service could give a wrong answer.
		var prevStation int64
		var prevDist, prevSecc, prevMesa string
		err = imp.getCitizen.QueryRow(citizenID, day, year, fn, sn1, sn2, postCode).Scan(&prevStation, &prevDist, &prevSecc, &prevMesa)
		if err != nil {
			return fmt.Errorf("line %d: checking duplicate: %w", line, err)
		}
		if prevStation == stationID && prevDist == place.dist && prevSecc == place.secc && prevMesa == place.mesa {
			imp.duplicates++
			return nil
		}
		if imp.collisions == 0 {
			imp.firstCollision = line
		}
		imp.collisions++
		return nil
	})
}

// pollingPlace identifies the table where a citizen votes.
type pollingPlace struct {
	station          stationKey
	dist, secc, mesa string
}

func pollingPlaceOf(field func(string) string) pollingPlace {
	var dir []string
	for _, c := range []string{"DIRMESA1", "DIRMESA2", "DIRMESA3", "DIRMESA4"} {
		if v := decodeField(field(c)); v != "" {
			dir = append(dir, v)
		}
	}
	return pollingPlace{
		station: stationKey{
			poblacion: decodeField(field("LMUN")),
			colele:    decodeField(field("NLOCAL")),
			dircol:    strings.Join(dir, " "),
		},
		dist: strings.TrimSpace(field("DIST")),
		secc: strings.TrimSpace(field("SECC")),
		mesa: strings.TrimSpace(field("MESA")),
	}
}

func (imp *importer) station(k stationKey) (int64, error) {
	if id, ok := imp.stations[k]; ok {
		return id, nil
	}
	res, err := imp.insStation.Exec(k.poblacion, k.colele, k.dircol)
	if err != nil {
		return 0, fmt.Errorf("inserting polling station: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	imp.stations[k] = id
	return id, nil
}
