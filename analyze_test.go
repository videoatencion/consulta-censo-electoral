// vim: set ts=4 sw=4 noet:
package main

import (
	"bytes"
	"hash/maphash"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every option the analysis proposes must import without collisions, and
// must stop doing so if it stored any less data.
func TestAnalysisOptionsAreMinimalAndImport(t *testing.T) {
	dir := t.TempDir()
	writeCensus(t, dir, rows)
	file := filepath.Join(dir, "censo.txt")

	c, err := loadCensus([]string{file})
	if err != nil {
		t.Fatal(err)
	}
	options := c.minimalOptions(3, 9)
	if len(options) == 0 {
		t.Fatal("no options found")
	}

	seed := maphash.MakeSeed()
	seen := map[uint64]int32{}
	for _, o := range options {
		doc := c.docHashes(o.mode, o.n, seed)
		if c.collides(doc, o.fields, o.nameChars, seen) {
			t.Errorf("%s: collides", o.env())
		}
		if o.n > o.mode.minN && c.collides(c.docHashes(o.mode, o.n-1, seed), o.fields, o.nameChars, seen) == false {
			t.Errorf("%s: one document character less would also do", o.env())
		}
		for i := range o.fields {
			fewer := append(append([]string{}, o.fields[:i]...), o.fields[i+1:]...)
			nc := o.nameChars
			hasName := false
			for _, f := range fewer {
				hasName = hasName || nameFields[f]
			}
			if !hasName {
				nc = 0
			}
			if !c.collides(doc, fewer, nc, seen) {
				t.Errorf("%s: field %s is not needed", o.env(), o.fields[i])
			}
		}
		if o.nameChars > 1 && !c.collides(doc, o.fields, o.nameChars-1, seen) {
			t.Errorf("%s: one name letter less would also do", o.env())
		}

		// The real import agrees.
		cfg := testConfig(t)
		cfg.DocumentChars, cfg.FirstChars, cfg.FirstCharsAddLetter = o.n, o.mode.first, o.mode.addLetter
		cfg.Day, cfg.Sn1 = false, false
		for _, f := range o.fields {
			switch f {
			case "day":
				cfg.Day = true
			case "year":
				cfg.Year = true
			case "fn":
				cfg.Fn = true
			case "sn1":
				cfg.Sn1 = true
			case "sn2":
				cfg.Sn2 = true
			case "postCode":
				cfg.PostCode = true
			}
		}
		if o.nameChars > 0 {
			cfg.NameChars = o.nameChars
		}
		if err := buildDatabase(cfg, filepath.Join(t.TempDir(), "x.db"), []string{file}); err != nil {
			t.Errorf("%s: import fails: %v", o.env(), err)
		}
	}
}

func TestAnalyzeCommandLeavesCensusUntouched(t *testing.T) {
	dir := t.TempDir()
	writeCensus(t, dir, rows)
	t.Setenv("DATA_DIR", dir)
	before, _ := os.ReadDir(dir)

	var out bytes.Buffer
	if code := analyze(nil, &out); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "Censo analizado: 4 ciudadanos") || !strings.Contains(out.String(), "DOCUMENT_CHARS=") {
		t.Fatalf("unexpected report:\n%s", out.String())
	}
	// Aggregated figures only: no document nor surname of anybody.
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(out.String(), func(r rune) bool { return r == ' ' || r == '\n' || r == '.' || r == ',' }) {
		words[w] = true
	}
	for _, r := range rows {
		for _, v := range []string{r.ident, r.ape1, r.ape2, r.nombre} {
			if v != "" && words[v] {
				t.Fatalf("report leaks %q:\n%s", v, out.String())
			}
		}
	}
	after, _ := os.ReadDir(dir)
	if len(before) != len(after) {
		t.Fatalf("analysis changed the data directory: %v -> %v", before, after)
	}
	t.Log("\n" + out.String())
}
