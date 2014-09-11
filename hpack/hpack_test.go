// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package hpack

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestStaticTable(t *testing.T) {
	fromSpec := `
          +-------+-----------------------------+---------------+
          | 1     | :authority                  |               |
          | 2     | :method                     | GET           |
          | 3     | :method                     | POST          |
          | 4     | :path                       | /             |
          | 5     | :path                       | /index.html   |
          | 6     | :scheme                     | http          |
          | 7     | :scheme                     | https         |
          | 8     | :status                     | 200           |
          | 9     | :status                     | 204           |
          | 10    | :status                     | 206           |
          | 11    | :status                     | 304           |
          | 12    | :status                     | 400           |
          | 13    | :status                     | 404           |
          | 14    | :status                     | 500           |
          | 15    | accept-charset              |               |
          | 16    | accept-encoding             | gzip, deflate |
          | 17    | accept-language             |               |
          | 18    | accept-ranges               |               |
          | 19    | accept                      |               |
          | 20    | access-control-allow-origin |               |
          | 21    | age                         |               |
          | 22    | allow                       |               |
          | 23    | authorization               |               |
          | 24    | cache-control               |               |
          | 25    | content-disposition         |               |
          | 26    | content-encoding            |               |
          | 27    | content-language            |               |
          | 28    | content-length              |               |
          | 29    | content-location            |               |
          | 30    | content-range               |               |
          | 31    | content-type                |               |
          | 32    | cookie                      |               |
          | 33    | date                        |               |
          | 34    | etag                        |               |
          | 35    | expect                      |               |
          | 36    | expires                     |               |
          | 37    | from                        |               |
          | 38    | host                        |               |
          | 39    | if-match                    |               |
          | 40    | if-modified-since           |               |
          | 41    | if-none-match               |               |
          | 42    | if-range                    |               |
          | 43    | if-unmodified-since         |               |
          | 44    | last-modified               |               |
          | 45    | link                        |               |
          | 46    | location                    |               |
          | 47    | max-forwards                |               |
          | 48    | proxy-authenticate          |               |
          | 49    | proxy-authorization         |               |
          | 50    | range                       |               |
          | 51    | referer                     |               |
          | 52    | refresh                     |               |
          | 53    | retry-after                 |               |
          | 54    | server                      |               |
          | 55    | set-cookie                  |               |
          | 56    | strict-transport-security   |               |
          | 57    | transfer-encoding           |               |
          | 58    | user-agent                  |               |
          | 59    | vary                        |               |
          | 60    | via                         |               |
          | 61    | www-authenticate            |               |
          +-------+-----------------------------+---------------+
`
	bs := bufio.NewScanner(strings.NewReader(fromSpec))
	re := regexp.MustCompile(`\| (\d+)\s+\| (\S+)\s*\| (\S(.*\S)?)?\s+\|`)
	for bs.Scan() {
		l := bs.Text()
		if !strings.Contains(l, "|") {
			continue
		}
		m := re.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		i, err := strconv.Atoi(m[1])
		if err != nil {
			t.Errorf("Bogus integer on line %q", l)
			continue
		}
		if i < 1 || i > len(staticTable) {
			t.Errorf("Bogus index %d on line %q", i, l)
			continue
		}
		if got, want := staticTable[i-1].Name, m[2]; got != want {
			t.Errorf("header index %d name = %q; want %q", i, got, want)
		}
		if got, want := staticTable[i-1].Value, m[3]; got != want {
			t.Errorf("header index %d value = %q; want %q", i, got, want)
		}
	}
	if err := bs.Err(); err != nil {
		t.Error(err)
	}
}

func (d *Decoder) mustAt(idx int) HeaderField {
	if hf, ok := d.at(idx); !ok {
		panic(fmt.Sprintf("bogus index %d", idx))
	} else {
		return hf
	}
}

func TestDynamicTableAt(t *testing.T) {
	d := NewDecoder(4096, nil)
	at := d.mustAt
	if got, want := at(2), (HeaderField{":method", "GET"}); got != want {
		t.Errorf("at(2) = %q; want %q", got, want)
	}
	d.dynTab.add(HeaderField{"foo", "bar"})
	d.dynTab.add(HeaderField{"blake", "miz"})
	if got, want := at(len(staticTable)+1), (HeaderField{"blake", "miz"}); got != want {
		t.Errorf("at(dyn 1) = %q; want %q", got, want)
	}
	if got, want := at(len(staticTable)+2), (HeaderField{"foo", "bar"}); got != want {
		t.Errorf("at(dyn 2) = %q; want %q", got, want)
	}
	if got, want := at(3), (HeaderField{":method", "POST"}); got != want {
		t.Errorf("at(3) = %q; want %q", got, want)
	}
}

func TestDynamicTableSizeEvict(t *testing.T) {
	d := NewDecoder(4096, nil)
	if want := uint32(0); d.dynTab.size != want {
		t.Fatalf("size = %d; want %d", d.dynTab.size, want)
	}
	add := d.dynTab.add
	add(HeaderField{"blake", "eats pizza"})
	if want := uint32(15 + 32); d.dynTab.size != want {
		t.Fatalf("after pizza, size = %d; want %d", d.dynTab.size, want)
	}
	add(HeaderField{"foo", "bar"})
	if want := uint32(15 + 32 + 6 + 32); d.dynTab.size != want {
		t.Fatalf("after foo bar, size = %d; want %d", d.dynTab.size, want)
	}
	d.dynTab.setMaxSize(15 + 32 + 1 /* slop */)
	if want := uint32(6 + 32); d.dynTab.size != want {
		t.Fatalf("after setMaxSize, size = %d; want %d", d.dynTab.size, want)
	}
	if got, want := d.mustAt(len(staticTable)+1), (HeaderField{"foo", "bar"}); got != want {
		t.Errorf("at(dyn 1) = %q; want %q", got, want)
	}
	add(HeaderField{"long", strings.Repeat("x", 500)})
	if want := uint32(0); d.dynTab.size != want {
		t.Fatalf("after big one, size = %d; want %d", d.dynTab.size, want)
	}
}

func TestDecoderDecode(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want []HeaderField
	}{
		// Indexed Header Field
		// http://http2.github.io/http2-spec/compression.html#rfc.section.C.2.4
		{"C.2.4", []byte("\x82"), []HeaderField{{":method", "GET"}}},
	}
	for _, tt := range tests {
		d := NewDecoder(4096, nil)
		hf, err := d.Decode(tt.in)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if !reflect.DeepEqual(hf, tt.want) {
			t.Errorf("%s: Got %v; want %v", tt.name, hf, tt.want)
		}
	}
}

func TestHuffmanDecode(t *testing.T) {
	tests := []struct {
		inHex, want string
	}{
		{"f1e3 c2e5 f23a 6ba0 ab90 f4ff", "www.example.com"},
		{"a8eb 1064 9cbf", "no-cache"},
		{"25a8 49e9 5ba9 7d7f", "custom-key"},
		{"25a8 49e9 5bb8 e8b4 bf", "custom-value"},
		{"6402", "302"},
		{"aec3 771a 4b", "private"},
		{"d07a be94 1054 d444 a820 0595 040b 8166 e082 a62d 1bff", "Mon, 21 Oct 2013 20:13:21 GMT"},
		{"9d29 ad17 1863 c78f 0b97 c8e9 ae82 ae43 d3", "https://www.example.com"},
		{"9bd9 ab", "gzip"},
		{"94e7 821d d7f2 e6c7 b335 dfdf cd5b 3960 d5af 2708 7f36 72c1 ab27 0fb5 291f 9587 3160 65c0 03ed 4ee5 b106 3d50 07",
			"foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1"},
	}
	for i, tt := range tests {
		var buf bytes.Buffer
		in, err := hex.DecodeString(strings.Replace(tt.inHex, " ", "", -1))
		if err != nil {
			t.Errorf("%d. hex input error: %v", i, err)
			continue
		}
		if _, err := HuffmanDecode(&buf, in); err != nil {
			t.Errorf("%d. decode error: %v", i, err)
			continue
		}
		if got := buf.String(); tt.want != got {
			t.Errorf("%d. decode = %q; want %q", i, got, tt.want)
		}
	}
}

func TestReadVarInt(t *testing.T) {
	type res struct {
		i        uint64
		consumed int
		err      error
	}
	tests := []struct {
		n    byte
		p    []byte
		want res
	}{
		// Fits in a byte:
		{1, []byte{0}, res{0, 1, nil}},
		{2, []byte{2}, res{2, 1, nil}},
		{3, []byte{6}, res{6, 1, nil}},
		{4, []byte{14}, res{14, 1, nil}},
		{5, []byte{30}, res{30, 1, nil}},
		{6, []byte{62}, res{62, 1, nil}},
		{7, []byte{126}, res{126, 1, nil}},
		{8, []byte{254}, res{254, 1, nil}},

		// Doesn't fit in a byte:
		{1, []byte{1}, res{0, 0, nil}},
		{2, []byte{3}, res{0, 0, nil}},
		{3, []byte{7}, res{0, 0, nil}},
		{4, []byte{15}, res{0, 0, nil}},
		{5, []byte{31}, res{0, 0, nil}},
		{6, []byte{63}, res{0, 0, nil}},
		{7, []byte{127}, res{0, 0, nil}},
		{8, []byte{255}, res{0, 0, nil}},

		// Ignoring top bits:
		{5, []byte{255, 154, 10}, res{1337, 3, nil}}, // high dummy three bits: 111
		{5, []byte{159, 154, 10}, res{1337, 3, nil}}, // high dummy three bits: 100
		{5, []byte{191, 154, 10}, res{1337, 3, nil}}, // high dummy three bits: 101

		// Extra byte:
		{5, []byte{191, 154, 10, 2}, res{1337, 3, nil}}, // extra byte

		// Short a byte:
		{5, []byte{191, 154}, res{0, 0, nil}},

		// integer overflow:
		{1, []byte{255, 128, 128, 128, 128, 128, 128, 128, 128, 128, 128, 128, 128, 128, 128, 128}, res{0, 0, errVarintOverflow}},
	}
	for _, tt := range tests {
		i, consumed, err := readVarInt(tt.n, tt.p)
		got := res{i, consumed, err}
		if got != tt.want {
			t.Errorf("readVarInt(%d, %v ~ %x) = %+v; want %+v", tt.n, tt.p, tt.p, got, tt.want)
		}
	}
}
