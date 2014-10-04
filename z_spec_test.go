// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import (
	"encoding/xml"
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

type specCoverage map[specPart]bool

func readSection(sc specCoverage, d *xml.Decoder, sec []int) {
	sub := 0
	for {
		tk, err := d.Token()
		if err != nil {
			if err == io.EOF {
				return
			}
			panic(err)
		}
		switch v := tk.(type) {
		case xml.StartElement:
			if v.Name.Local == "section" {
				sub++
				readSection(sc, d, append(sec, sub))
			}
		case xml.CharData:
			if len(sec) == 0 {
				break
			}
			ssec := fmt.Sprintf("%d", sec[0])
			for _, n := range sec[1:] {
				ssec = fmt.Sprintf("%s.%d", ssec, n)
			}
			sc.addSentences(ssec, string(v))
		case xml.EndElement:
			if v.Name.Local == "section" {
				return
			}
		}
	}

}

func readSpecCov(r io.Reader) specCoverage {
	d := xml.NewDecoder(r)
	sc := specCoverage{}
	readSection(sc, d, nil)
	return sc
}

func (sc specCoverage) addSentences(sec string, sentence string) {
	for _, s := range parseSentences(sentence) {
		sc[specPart{sec, s}] = false
	}
}

func (sc specCoverage) cover(sec string, sentence string) {
	for _, s := range parseSentences(sentence) {
		p := specPart{sec, s}
		if _, ok := sc[p]; !ok {
			panic(fmt.Sprintf("Not found in spec: %q, %q", sec, s))
		}
		sc[specPart{sec, s}] = true
	}

}

func parseSentences(sens string) []string {
	sens = strings.TrimSpace(sens)
	if sens == "" {
		return nil
	}
	rx := regexp.MustCompile("[\t\n\r]")
	ss := strings.Split(rx.ReplaceAllString(sens, " "), ". ")
	for i, s := range ss {
		s = strings.TrimSpace(s)
		if !strings.HasSuffix(s, ".") {
			s += "."
		}
		ss[i] = s
	}
	return ss
}

func Test_Z_Spec_ParseSentences(t *testing.T) {
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

func Test_Z_BuildCoverageTable(t *testing.T) {
	testdata := `
<rfc>
  <middle>
    <section anchor="intro" title="Introduction">
      <t>Foo.</t>
      <t><t>Sentence 1.
      Sentence 2
   .	Sentence 3.</t></t>
    </section>
    <section anchor="bar" title="Introduction">
      <t>Bar.</t>
	<section anchor="bar" title="Introduction">
	  <t>Baz.</t>
	</section>
    </section>
  </middle>
</rfc>`
	got := readSpecCov(strings.NewReader(testdata))
	want := specCoverage{
		specPart{"1", "Foo."}:        false,
		specPart{"1", "Sentence 1."}: false,
		specPart{"1", "Sentence 2."}: false,
		specPart{"1", "Sentence 3."}: false,
		specPart{"2", "Bar."}:        false,
		specPart{"2.1", "Baz."}:      false,
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %+v, want %+v", got, want)
	}
}

func Test_Z_SpecUncovered(t *testing.T) {
	testdata := `
<rfc>
  <middle>
    <section anchor="intro" title="Introduction">
	<t>Foo.</t>
	<t><t>Sentence 1.</t></t>
    </section>
  </middle>
</rfc>`
	sp := readSpecCov(strings.NewReader(testdata))
	sp.cover("1", "Foo. Sentence 1.")

	want := specCoverage{
		specPart{"1", "Foo."}:        true,
		specPart{"1", "Sentence 1."}: true,
	}

	if !reflect.DeepEqual(sp, want) {
		t.Errorf("got = %+v, want %+v", sp, want)
	}

	defer func() {
		if err := recover(); err == nil {
			t.Error("expected panic")
		}
	}()

	sp.cover("1", "Not in spec.")
}

func TestSpecCoverage(t *testing.T) {
	var notCovered bySpecSection
	for p, covered := range defaultSpecCoverage {
		if !covered {
			notCovered = append(notCovered, p)
		}
	}
	if len(notCovered) == 0 {
		return
	}
	sort.Sort(notCovered)

	const shortLen = 5
	if testing.Short() && len(notCovered) > shortLen {
		notCovered = notCovered[:shortLen]
	}
	t.Logf("COVER REPORT:")
	for _, p := range notCovered {
		t.Errorf("\tSECTION %s: %s", p.section, p.sentence)
	}
}
