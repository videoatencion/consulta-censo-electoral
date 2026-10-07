// vim: set ts=4 sw=4 noet:
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
)

const header = `"NIE";"CPRO";"LMUN";"DIST";"SECC";"MESA";"NLOCAL";"NLOCALB";"INFADICIONAL";"DIRMESA1";"DIRMESA2";"DIRMESA3";"DIRMESA4";"NOMBRE";"APE1";"APE2";"DOMI1";"DOMI2";"DOMI3";"ENTI1";"ENTI2";"ENTI3";"CPOSTAL";"CPRON";"CNMUN";"FNAC";"SEXO";"IDENT";"CPOSTAM";"NIA";"GESCO";"NORDEN";"NACIONALIDAD";"INTENCIONVOTO";`

type row struct {
	lmun, dist, secc, mesa, nlocal, dir1, dir2 string
	nombre, ape1, ape2, fnac, ident, cpostam   string
}

func (r row) csv() string {
	f := make([]string, 34)
	f[2], f[3], f[4], f[5], f[6], f[9], f[10] = r.lmun, r.dist, r.secc, r.mesa, r.nlocal, r.dir1, r.dir2
	f[13], f[14], f[15], f[25], f[27], f[28] = r.nombre, r.ape1, r.ape2, r.fnac, r.ident, r.cpostam
	for i := range f {
		f[i] = `"` + f[i] + `"`
	}
	return strings.Join(f, ";") + ";"
}

// Fictitious citizens. 11111111H and 22211111H share their last 5 chars.
var rows = []row{
	{"RUBÍ", "01", "001", "A", "ESCOLA RAMON LLULL", "AV FLORS", "43", "JOAN", "ÁLVAREZ", "MARTÍ", "31/12/1991", "11111111H", "08191"},
	{"RUBÍ", "02", "003", "B", "INSTITUT L'ESTATGE", "C. MAJOR", "1", "MARIA", "PUIG", "SOLÀ", "05/03/1980", "22211111H", "08191"},
	{"TERRASSA", "01", "002", "U", "ESCOLA PÚBLICA", "PL. NOVA", "", "PERE", "FONT", "ROCA", "07/07/1970", "33333333P", "08221"},
	{"SANT CUGAT", "01", "002", "U", "ESCOLA PÚBLICA", "C. DEL MIG", "2", "ANNA", "VILA", "COLL", "08/08/1975", "44444444A", "08172"},
	{"RUBÍ", "01", "001", "A", "ESCOLA RAMON LLULL", "AV FLORS", "43", "SENSE", "DOCUMENT", "", "01/01/1990", "", "08191"},
}

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		DataDir: t.TempDir(), Token: "secret", Location: time.UTC,
		DocumentChars: 5, NameChars: 2, Day: true, Sn1: true,
	}
}

func writeCensus(t *testing.T, dir string, rows []row) {
	t.Helper()
	lines := []string{header}
	for _, r := range rows {
		lines = append(lines, r.csv())
	}
	latin1, err := charmap.ISO8859_1.NewEncoder().String(strings.Join(lines, "\r\n") + "\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "censo.txt"), []byte(latin1), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadTestDB(t *testing.T, cfg Config) *sql.DB {
	t.Helper()
	writeCensus(t, cfg.DataDir, rows)
	db, err := prepareDatabase(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestImportRemovesCSVAndReopens(t *testing.T) {
	cfg := testConfig(t)
	loadTestDB(t, cfg)

	if _, err := os.Stat(filepath.Join(cfg.DataDir, "censo.txt")); !os.IsNotExist(err) {
		t.Fatalf("census file should be removed after import, stat err = %v", err)
	}
	db, err := prepareDatabase(cfg)
	if err != nil {
		t.Fatalf("reopening existing database: %v", err)
	}
	defer db.Close()
	var n int
	db.QueryRow("SELECT COUNT(*) FROM citizens").Scan(&n)
	if n != 4 {
		t.Fatalf("got %d citizens, want 4", n)
	}
}

func TestLookup(t *testing.T) {
	cfg := testConfig(t)
	db := loadTestDB(t, cfg)

	var holder atomic.Pointer[sql.DB]
	holder.Store(db)
	router := newRouter(cfg, &holder)

	tests := []struct {
		name, body, wantColele, wantDir, wantErr string
	}{
		{"ambiguous document asks for more fields", `{"citizenId":"1111H"}`, "", "", "[day sn1]"},
		{"full document and accented surname", `{"citizenId":"11111111-h","sn1":"Álvarez"}`, "ESCOLA RAMON LLULL", "AV FLORS 43", ""},
		{"one digit day", `{"citizenId":"1111H","day":"5"}`, "INSTITUT L'ESTATGE", "C. MAJOR 1", ""},
		{"same school name in two towns", `{"citizenId":"44444444A"}`, "ESCOLA PÚBLICA", "C. DEL MIG 2", ""},
		{"fields not indexed are ignored", `{"citizenId":"4444A","sn2":"XX","fn":"XX"}`, "ESCOLA PÚBLICA", "C. DEL MIG 2", ""},
		{"not found", `{"citizenId":"99999Z"}`, "", "", "no records found"},
		{"missing document", `{"day":"01"}`, "", "", "invalid request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/consulta", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "secret")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var got CitizenInfo
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" {
				if !strings.HasPrefix(got.ErrorMessage, tt.wantErr) {
					t.Fatalf("errorMessage = %q, want %q", got.ErrorMessage, tt.wantErr)
				}
				return
			}
			if got.ErrorMessage != "" || got.Colele != tt.wantColele || got.Dircol != tt.wantDir {
				t.Fatalf("got %+v, want colele %q dircol %q", got, tt.wantColele, tt.wantDir)
			}
		})
	}
}

func TestAuthAndHealth(t *testing.T) {
	cfg := testConfig(t)
	var holder atomic.Pointer[sql.DB]
	router := newRouter(cfg, &holder)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("health before load = %d, want 503", w.Code)
	}

	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/consulta", strings.NewReader(`{"citizenId":"1"}`))
	req.Header.Set("Authorization", "wrong")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("bad token = %d, want 403", w.Code)
	}

	holder.Store(loadTestDB(t, cfg))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("health after load = %d, want 200", w.Code)
	}
}

func TestCollisionAbortsImport(t *testing.T) {
	cfg := testConfig(t)
	cfg.Day, cfg.Sn1 = false, false
	writeCensus(t, cfg.DataDir, rows)

	if _, err := prepareDatabase(cfg); err == nil || !strings.Contains(err.Error(), "collisions") {
		t.Fatalf("want collision error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "censo.txt")); err != nil {
		t.Fatalf("census file must be kept when the import fails: %v", err)
	}
	if entries, _ := os.ReadDir(cfg.DataDir); len(entries) != 1 {
		t.Fatalf("failed import left files behind: %v", entries)
	}
}

func TestDocumentKey(t *testing.T) {
	tests := []struct {
		first, letter bool
		chars         int
		in, want      string
	}{
		{false, false, 5, "12345678A", "5678A"},
		{false, false, 5, "5678a", "5678A"},
		{true, false, 5, "12345678A", "12345"},
		{true, true, 5, "12345678-A", "12345A"},
		{true, true, 5, "12345A", "12345A"},
		{false, false, 0, "x1234567l", "X1234567L"},
		{true, false, 5, "123", "123"},
	}
	for _, tt := range tests {
		cfg := Config{DocumentChars: tt.chars, FirstChars: tt.first, FirstCharsAddLetter: tt.letter}
		if got := cfg.documentKey(tt.in); got != tt.want {
			t.Errorf("documentKey(%q) first=%v letter=%v = %q, want %q", tt.in, tt.first, tt.letter, got, tt.want)
		}
	}
}

func TestNameKey(t *testing.T) {
	cfg := Config{NameChars: 3}
	for in, want := range map[string]string{"Àlvarez": "ALV", "çanadell": "CAN", " Ñu ": "NU", "Ò": "O"} {
		if got := cfg.nameKey(in); got != want {
			t.Errorf("nameKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindDifferingFieldsRanking(t *testing.T) {
	p1 := CitizenInfo{Poblacion: "RUBÍ", Colele: "ESCOLA A", Mesa: "A"}
	p2 := CitizenInfo{Poblacion: "RUBÍ", Colele: "ESCOLA B", Mesa: "A"}
	infos := []CitizenInfo{p1, p2, p2, p1}
	// sn1 and year settle the table on their own (year is easier to answer);
	// day and fn split the citizens but every group still holds both tables,
	// and postCode is the same for all: none of them is worth asking for.
	keys := []CitizenKey{
		{Day: "01", Year: "80", Sn1: "AL", Fn: "JO", PostCode: "08191"},
		{Day: "01", Year: "90", Sn1: "PU", Fn: "JO", PostCode: "08191"},
		{Day: "02", Year: "90", Sn1: "PU", Fn: "MA", PostCode: "08191"},
		{Day: "02", Year: "81", Sn1: "AL", Fn: "MA", PostCode: "08191"},
	}
	if got := fmt.Sprint(findDifferingFields(keys, infos)); got != "[year sn1]" {
		t.Fatalf("got %s, want [year sn1]", got)
	}

	// A field that only narrows the choice comes after one that settles it.
	keys[3].Year = "90"
	if got := fmt.Sprint(findDifferingFields(keys, infos)); got != "[sn1 year]" {
		t.Fatalf("got %s, want [sn1 year]", got)
	}

	// Nothing the citizen can tell helps: only the polling station does.
	for i := range keys {
		keys[i].Sn1, keys[i].Year = "AL", "90"
	}
	if got := fmt.Sprint(findDifferingFields(keys, infos)); got != "[colele]" {
		t.Fatalf("got %s, want [colele]", got)
	}
}

func TestFormato(t *testing.T) {
	cfg := testConfig(t)
	cfg.FirstChars, cfg.FirstCharsAddLetter = true, true
	var holder atomic.Pointer[sql.DB]
	router := newRouter(cfg, &holder)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/formato", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("without token = %d, want 403", w.Code)
	}

	// Answered even while the census is still loading.
	req := httptest.NewRequest(http.MethodGet, "/formato", nil)
	req.Header.Set("Authorization", "secret")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != `{"addLetter":true,"documentChars":5,"firstChars":true}` {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}
