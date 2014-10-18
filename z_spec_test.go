// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var coverSpec = flag.Bool("coverspec", false, "Run spec coverage tests")

// The global map of sentence coverage for the http2 spec.
var defaultSpecCoverage specCoverage

func init() {
	f, err := os.Open("testdata/draft-ietf-httpbis-http2.xml")
	if err != nil {
		panic(err)
	}
	defaultSpecCoverage = readSpecCov(f)
}

// specCover marks all sentences for section sec in defaultSpecCoverage. Sentences not
// "covered" will be included in report outputed by TestSpecCoverage.
func specCover(sec, sentences string) {
	defaultSpecCoverage.cover(sec, sentences)
}

type specPart struct {
	section  string
	sentence string
}

func (ss specPart) Less(oo specPart) bool {
	atoi := func(s string) int {
		n, err := strconv.Atoi(s)
		if err != nil {
			panic(err)
		}
		return n
	}
	a := strings.Split(ss.section, ".")
	b := strings.Split(oo.section, ".")
	for i := 0; i < len(a); i++ {
		if i >= len(b) {
			return false
		}
		x, y := atoi(a[i]), atoi(b[i])
		if x < y {
			return true
		}
	}
	return false
}

type bySpecSection []specPart

func (a bySpecSection) Len() int           { return len(a) }
func (a bySpecSection) Less(i, j int) bool { return a[i].Less(a[j]) }
func (a bySpecSection) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

type specCoverage struct {
	m map[specPart]bool
	d *xml.Decoder
}

func joinSection(sec []int) string {
	s := fmt.Sprintf("%d", sec[0])
	for _, n := range sec[1:] {
		s = fmt.Sprintf("%s.%d", s, n)
	}
	return s
}

func (sc specCoverage) readSection(sec []int) {
	var (
		buf = new(bytes.Buffer)
		sub = 0
	)
	for {
		tk, err := sc.d.Token()
		if err != nil {
			if err == io.EOF {
				return
			}
			panic(err)
		}
		switch v := tk.(type) {
		case xml.StartElement:
			if skipElement(v) {
				if err := sc.d.Skip(); err != nil {
					panic(err)
				}
				break
			}
			switch v.Name.Local {
			case "section":
				sub++
				sc.readSection(append(sec, sub))
			case "xref":
				buf.Write(sc.readXRef(v))
			}
		case xml.CharData:
			if len(sec) == 0 {
				break
			}
			buf.Write(v)
		case xml.EndElement:
			if v.Name.Local == "section" {
				sc.addSentences(joinSection(sec), buf.String())
				return
			}
		}
	}
}

func (sc specCoverage) readXRef(se xml.StartElement) []byte {
	var b []byte
	for {
		tk, err := sc.d.Token()
		if err != nil {
			panic(err)
		}
		switch v := tk.(type) {
		case xml.CharData:
			if b != nil {
				panic("unexpected CharData")
			}
			b = []byte(v)
		case xml.EndElement:
			if v.Name.Local != "xref" {
				panic("expected </xref>")
			}
			if b != nil {
				return b
			}
			return []byte(fmt.Sprintf("%#v", se))
		default:
			panic(fmt.Sprintf("unexpected tag %q", v))
		}
	}
}

var skipAnchor = map[string]bool{
	"intro":    true,
	"Overview": true,
}

var skipTitle = map[string]bool{
	"Acknowledgements":            true,
	"Change Log":                  true,
	"Document Organization":       true,
	"Conventions and Terminology": true,
}

func skipElement(s xml.StartElement) bool {
	switch s.Name.Local {
	case "artwork":
		return true
	case "section":
		for _, attr := range s.Attr {
			switch attr.Name.Local {
			case "anchor":
				if skipAnchor[attr.Value] || strings.HasPrefix(attr.Value, "changes.since.") {
					return true
				}
			case "title":
				if skipTitle[attr.Value] {
					return true
				}
			}
		}
	}
	return false
}

func readSpecCov(r io.Reader) specCoverage {
	sc := specCoverage{
		m: map[specPart]bool{},
		d: xml.NewDecoder(r)}
	sc.readSection(nil)
	return sc
}

func (sc specCoverage) addSentences(sec string, sentence string) {
	for _, s := range parseSentences(sentence) {
		sc.m[specPart{sec, s}] = false
	}
}

func (sc specCoverage) cover(sec string, sentence string) {
	for _, s := range parseSentences(sentence) {
		p := specPart{sec, s}
		if _, ok := sc.m[p]; !ok {
			panic(fmt.Sprintf("Not found in spec: %q, %q", sec, s))
		}
		sc.m[specPart{sec, s}] = true
	}

}

func (sc specCoverage) uncovered() []specPart {
	var a []specPart
	for p, covered := range sc.m {
		if !covered {
			a = append(a, p)
		}
	}
	return a
}

var whitespaceRx = regexp.MustCompile(`\s+`)

func parseSentences(sens string) []string {
	sens = strings.TrimSpace(sens)
	if sens == "" {
		return nil
	}
	ss := strings.Split(whitespaceRx.ReplaceAllString(sens, " "), ". ")
	for i, s := range ss {
		s = strings.TrimSpace(s)
		if !strings.HasSuffix(s, ".") {
			s += "."
		}
		ss[i] = s
	}
	return ss
}

func TestSpecParseSentences(t *testing.T) {
	tests := []struct {
		ss   string
		want []string
	}{
		{"Sentence 1. Sentence 2.",
			[]string{
				"Sentence 1.",
				"Sentence 2.",
			}},
		{"Sentence 1.  \nSentence 2.\tSentence 3.",
			[]string{
				"Sentence 1.",
				"Sentence 2.",
				"Sentence 3.",
			}},
	}

	for i, tt := range tests {
		got := parseSentences(tt.ss)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%d: got = %q, want %q", i, got, tt.want)
		}
	}
}

func TestSpecCoverage(t *testing.T) {
	if !*coverSpec {
		t.Skip()
	}
	uncovered := defaultSpecCoverage.uncovered()
	if len(uncovered) == 0 {
		return
	}
	sort.Sort(bySpecSection(uncovered))

	const shortLen = 5
	if testing.Short() && len(uncovered) > shortLen {
		uncovered = uncovered[:shortLen]
	}
	t.Logf("COVER REPORT:")
	fails := 0
	for _, p := range uncovered {
		t.Errorf("\tSECTION %s: %s", p.section, p.sentence)
		fails++
	}
	t.Logf("%d sections not covered", fails)
}
