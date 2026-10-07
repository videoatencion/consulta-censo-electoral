// vim: set ts=4 sw=4 noet:
package main

import (
	"errors"
	"flag"
	"fmt"
	"hash/maphash"
	"io"
	"math"
	"sort"
	"strings"
)

// The "analizar" command reads a census without importing nor deleting it and
// finds the least personal data that still tells apart every citizen voting at
// a different table, using the same normalization as the import. It only
// prints aggregated figures, never data about anybody.

type docMode struct {
	first, addLetter bool
	minN, maxN       int
}

// For a DNI (8 digits + letter) the largest values keep the whole document.
var docModes = []docMode{
	{false, false, 3, 9},
	{true, false, 3, 8},
	{true, true, 3, 8},
}

var nameFields = map[string]bool{"fn": true, "sn1": true, "sn2": true}

const maxNameChars = 3

type analysisRow struct {
	doc    string
	dateOK bool
	place  int32
}

type census struct {
	rows      []analysisRow
	fields    map[string][maxNameChars + 1][]uint64 // column -> name chars -> hash per row (index 0 for non-name fields)
	places    int
	noDocRows int
	badDates  int
}

type indexOption struct {
	mode      docMode
	n         int
	fields    []string // in keyFields order
	nameChars int      // 0 when no name field is indexed
	excluded  int      // citizens left out for lack of a valid birthdate
	docOnly   float64  // share resolved with the document alone
	bits      float64  // estimated personal information stored per citizen
}

func analyze(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("analizar", flag.ContinueOnError)
	fs.SetOutput(out)
	maxFields := fs.Int("max-campos", 3, "número máximo de campos de desempate por opción")
	maxOptions := fs.Int("opciones", 10, "número de opciones que se muestran")
	maxDoc := fs.Int("max-documento", 6, "número máximo de caracteres del documento (el DNI casi entero es un identificador directo)")
	fs.Usage = func() {
		fmt.Fprintln(out, "Uso: censoElectoral analizar [-max-campos N] [-max-documento N] [-opciones N] [fichero.csv ...]")
		fmt.Fprintln(out, "Sin ficheros, analiza los .csv/.txt de DATA_DIR (por defecto /data). No modifica ni borra nada.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	files := fs.Args()
	if len(files) == 0 {
		var err error
		if files, _, err = listDataDir(envString("DATA_DIR", "/data")); err != nil {
			fmt.Fprintln(out, err)
			return 1
		}
	}
	if len(files) == 0 {
		fmt.Fprintln(out, "No se ha encontrado ningún fichero de censo (.csv o .txt).")
		return 1
	}

	c, err := loadCensus(files)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	options := c.minimalOptions(*maxFields, *maxDoc)
	c.report(out, options, *maxOptions, *maxFields, *maxDoc)
	return 0
}

func loadCensus(files []string) (*census, error) {
	seed := maphash.MakeSeed()
	c := &census{fields: map[string][maxNameChars + 1][]uint64{}}
	places := map[pollingPlace]int32{}
	hashes := map[string]*[maxNameChars + 1][]uint64{}
	for _, f := range keyFields {
		hashes[f.column] = &[maxNameChars + 1][]uint64{}
	}
	appendHash := func(column string, nc int, v string) {
		h := hashes[column]
		h[nc] = append(h[nc], maphash.String(seed, v))
	}

	for _, path := range files {
		err := readCensus(path, func(line int, field func(string) string) error {
			doc := normalizeDocument(field("IDENT"))
			if doc == "" {
				c.noDocRows++
				return nil
			}
			place := pollingPlaceOf(field)
			id, ok := places[place]
			if !ok {
				id = int32(len(places))
				places[place] = id
			}
			day, year, dateOK := birthdateKeys(field("FNAC"))
			if !dateOK {
				c.badDates++
			}
			c.rows = append(c.rows, analysisRow{doc: doc, dateOK: dateOK, place: id})

			appendHash("day", 0, day)
			appendHash("year", 0, year)
			appendHash("postCode", 0, strings.TrimSpace(field("CPOSTAM")))
			for column, name := range map[string]string{"fn": "NOMBRE", "sn1": "APE1", "sn2": "APE2"} {
				full := normalizeName(decodeField(field(name)))
				for nc := 1; nc <= maxNameChars; nc++ {
					appendHash(column, nc, truncateUTF8String(full, nc))
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	for column, h := range hashes {
		c.fields[column] = *h
	}
	c.places = len(places)
	return c, nil
}

// docHashes returns the hash of the indexed part of every document.
func (c *census) docHashes(mode docMode, n int, seed maphash.Seed) []uint64 {
	h := make([]uint64, len(c.rows))
	for i, r := range c.rows {
		h[i] = maphash.String(seed, reduceDocument(r.doc, n, mode.first, mode.addLetter))
	}
	return h
}

// collides tells whether two citizens with the same key vote at different
// tables, which would make the import abort. Rows without a valid birthdate
// are left out when the birthdate is indexed, as the import does.
func (c *census) collides(doc []uint64, fields []string, nc int, seen map[uint64]int32) bool {
	clear(seen)
	needDate := false
	var cols [][]uint64
	for _, f := range fields {
		if f == "day" || f == "year" {
			needDate = true
		}
		if nameFields[f] {
			cols = append(cols, c.fields[f][nc])
		} else {
			cols = append(cols, c.fields[f][0])
		}
	}
	for i, r := range c.rows {
		if needDate && !r.dateOK {
			continue
		}
		h := doc[i]
		for _, col := range cols {
			h = (h ^ col[i]) * 0x100000001b3
		}
		if p, ok := seen[h]; ok {
			if p != r.place {
				return true
			}
		} else {
			seen[h] = r.place
		}
	}
	return false
}

// minimalOptions finds, for every document mode and set of tie-breaking
// fields, the fewest document characters that avoid collisions, and keeps
// only the options that no other option beats with less data.
func (c *census) minimalOptions(maxFields, maxDoc int) []indexOption {
	type combo struct {
		fields []string
		nc     int
	}
	var combos []combo
	columns := make([]string, len(keyFields))
	for i, f := range keyFields {
		columns[i] = f.column
	}
	for mask := 0; mask < 1<<len(columns); mask++ {
		var fields []string
		hasName := false
		for i, col := range columns {
			if mask&(1<<i) != 0 {
				fields = append(fields, col)
				hasName = hasName || nameFields[col]
			}
		}
		if len(fields) > maxFields {
			continue
		}
		if !hasName {
			combos = append(combos, combo{fields, 0})
			continue
		}
		for nc := 1; nc <= maxNameChars; nc++ {
			combos = append(combos, combo{fields, nc})
		}
	}

	seed := maphash.MakeSeed()
	seen := make(map[uint64]int32, len(c.rows))
	var options []indexOption
	for _, mode := range docModes {
		found := make([]bool, len(combos))
		for n := mode.minN; n <= min(mode.maxN, maxDoc); n++ {
			doc := c.docHashes(mode, n, seed)
			for i, cb := range combos {
				if found[i] || c.collides(doc, cb.fields, cb.nc, seen) {
					continue
				}
				found[i] = true
				options = append(options, indexOption{mode: mode, n: n, fields: cb.fields, nameChars: cb.nc})
			}
		}
	}

	options = dropDominated(options)
	for i := range options {
		o := &options[i]
		for _, f := range o.fields {
			if f == "day" || f == "year" {
				o.excluded = c.badDates
			}
		}
		o.docOnly = c.resolvedByDocument(c.docHashes(o.mode, o.n, seed))
		o.bits = c.bits(*o)
	}
	sort.SliceStable(options, func(i, j int) bool {
		a, b := options[i], options[j]
		if math.Abs(a.bits-b.bits) > 0.05 {
			return a.bits < b.bits
		}
		if len(a.fields) != len(b.fields) {
			return len(a.fields) < len(b.fields)
		}
		if a.excluded != b.excluded {
			return a.excluded < b.excluded
		}
		return a.docOnly > b.docOnly
	})
	return options
}

// dropDominated removes an option when another one with the same document
// mode needs a subset of its fields, no more document characters and no more
// name characters.
func dropDominated(options []indexOption) []indexOption {
	var kept []indexOption
	for i, b := range options {
		dominated := false
		for j, a := range options {
			if i == j || a.mode != b.mode || a.n > b.n || !isSubset(a.fields, b.fields) {
				continue
			}
			if a.nameChars > 0 && a.nameChars > b.nameChars {
				continue
			}
			if a.n == b.n && len(a.fields) == len(b.fields) && a.nameChars == b.nameChars {
				continue
			}
			dominated = true
			break
		}
		if !dominated {
			kept = append(kept, b)
		}
	}
	return kept
}

func isSubset(a, b []string) bool {
	for _, x := range a {
		found := false
		for _, y := range b {
			if x == y {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// bits estimates how much personal information an option stores for each
// citizen: 3.3 bits per document digit (log2 10), 4.5 for the DNI letter when
// it cannot be derived from the stored digits (log2 23), and the entropy that
// each tie-breaking field actually has in this census (a postal code says
// little within one municipality; a year of birth says much more).
func (c *census) bits(o indexOption) float64 {
	digits, letter := o.n, false
	switch {
	case o.mode.first && o.mode.addLetter:
		letter = true
	case !o.mode.first:
		digits, letter = o.n-1, o.n < 9
	}
	bits := float64(digits) * math.Log2(10)
	if letter {
		bits += math.Log2(23)
	}
	for _, f := range o.fields {
		nc := 0
		if nameFields[f] {
			nc = o.nameChars
		}
		bits += entropy(c.fields[f][nc])
	}
	return bits
}

func entropy(values []uint64) float64 {
	counts := map[uint64]int{}
	for _, v := range values {
		counts[v]++
	}
	var h float64
	for _, n := range counts {
		p := float64(n) / float64(len(values))
		h -= p * math.Log2(p)
	}
	return h
}

// resolvedByDocument is the share of citizens whose table is known from the
// indexed part of the document alone, without any other question.
func (c *census) resolvedByDocument(doc []uint64) float64 {
	place := make(map[uint64]int32, len(c.rows))
	for i, r := range c.rows {
		if p, ok := place[doc[i]]; !ok {
			place[doc[i]] = r.place
		} else if p != r.place {
			place[doc[i]] = -1
		}
	}
	resolved := 0
	for i := range c.rows {
		if place[doc[i]] >= 0 {
			resolved++
		}
	}
	return float64(resolved) / float64(len(c.rows)) * 100
}

func (o indexOption) describe() string {
	var doc string
	switch {
	case o.mode.first && o.mode.addLetter:
		doc = fmt.Sprintf("las primeras %d cifras y la letra del documento", o.n)
	case o.mode.first:
		doc = fmt.Sprintf("las primeras %d cifras del documento", o.n)
	case o.n >= 9:
		doc = "el documento entero"
	default:
		doc = fmt.Sprintf("los últimos %d caracteres del documento (%d cifras y la letra en un DNI)", o.n, o.n-1)
	}

	letters := func(n int) string {
		if n == 1 {
			return "la primera letra"
		}
		return fmt.Sprintf("las %d primeras letras", n)
	}
	parts := []string{doc}
	for _, f := range o.fields {
		switch f {
		case "day":
			parts = append(parts, "el día de nacimiento")
		case "year":
			parts = append(parts, "el año de nacimiento")
		case "sn1":
			parts = append(parts, letters(o.nameChars)+" del primer apellido")
		case "sn2":
			parts = append(parts, letters(o.nameChars)+" del segundo apellido")
		case "fn":
			parts = append(parts, letters(o.nameChars)+" del nombre")
		case "postCode":
			parts = append(parts, "el código postal")
		}
	}
	if len(parts) == 1 {
		return strings.ToUpper(doc[:1]) + doc[1:]
	}
	s := strings.Join(parts[:len(parts)-1], ", ") + " y " + parts[len(parts)-1]
	return strings.ToUpper(s[:1]) + s[1:]
}

func (o indexOption) env() string {
	vars := []string{fmt.Sprintf("DOCUMENT_CHARS=%d", o.n)}
	if o.mode.first {
		vars = append(vars, "FIRST_CHARS=true")
	}
	if o.mode.addLetter {
		vars = append(vars, "FIRST_CHARS_ADD_LETTER=true")
	}
	names := map[string]string{"day": "DAY", "year": "YEAR", "fn": "FN", "sn1": "SN1", "sn2": "SN2", "postCode": "POST_CODE"}
	for _, f := range o.fields {
		vars = append(vars, names[f]+"=true")
	}
	if o.nameChars > 0 {
		vars = append(vars, fmt.Sprintf("NAME_CHARS=%d", o.nameChars))
	}
	return strings.Join(vars, " ")
}

func (c *census) report(out io.Writer, options []indexOption, maxOptions, maxFields, maxDoc int) {
	fmt.Fprintf(out, "Censo analizado: %s ciudadanos que votan en %s mesas.\n", thousands(len(c.rows)), thousands(c.places))
	if c.noDocRows == 1 {
		fmt.Fprintln(out, "1 fila sin documento no se puede indexar.")
	} else if c.noDocRows > 1 {
		fmt.Fprintf(out, "%s filas sin documento no se pueden indexar.\n", thousands(c.noDocRows))
	}
	fmt.Fprintln(out)

	if len(options) == 0 {
		fmt.Fprintf(out, "Ninguna combinación de hasta %d campos de desempate y %d caracteres del documento evita las colisiones.\nPruebe con más campos (-max-campos %d) o, si no queda más remedio, más caracteres del documento (-max-documento).\n", maxFields, maxDoc, maxFields+1)
		return
	}

	fmt.Fprintln(out, "Opciones mínimas sin colisiones, de menos a más información personal guardada:")
	for i, o := range options {
		if i == maxOptions {
			fmt.Fprintf(out, "\n(%d opciones más; use -opciones %d para verlas todas)\n", len(options)-maxOptions, len(options))
			break
		}
		fmt.Fprintf(out, "\n%2d. %s.\n", i+1, o.describe())
		fmt.Fprintf(out, "    %s\n", o.env())
		fmt.Fprintf(out, "    Información guardada por ciudadano: unos %s bits.\n", strings.Replace(fmt.Sprintf("%.0f", o.bits), ".", ",", 1))
		if o.docOnly >= 100 {
			fmt.Fprintln(out, "    Sólo con el documento se resuelven todos los ciudadanos, sin ninguna pregunta más.")
		} else {
			fmt.Fprintf(out, "    Sólo con el documento se resuelve el %s %% de los ciudadanos; el resto tendrá que responder alguna pregunta más.\n", percent(o.docOnly))
		}
		if o.excluded > 0 {
			fmt.Fprintf(out, "    Atención: %s ciudadanos sin fecha de nacimiento válida quedarían fuera del índice.\n", thousands(o.excluded))
		}
	}
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "." + s[i:]
	}
	return s
}

func percent(p float64) string {
	return strings.Replace(fmt.Sprintf("%.1f", p), ".", ",", 1)
}
