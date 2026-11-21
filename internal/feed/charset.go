package feed

import (
	"io"
	"strings"
	"unicode/utf8"
)

// cp1252 maps bytes 0x80–0x9F; the rest of Windows-1252 and all of ISO-8859-1 equal their code points.
var cp1252 = [32]rune{
	0x20AC, 0xFFFD, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0xFFFD, 0x017D, 0xFFFD,
	0xFFFD, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0xFFFD, 0x017E, 0x0178,
}

var latin1 = func() *[256]rune {
	var t [256]rune
	for i := range t {
		t[i] = rune(i)
	}
	return &t
}()

var windows1252 = func() *[256]rune {
	t := *latin1
	for i, r := range cp1252 {
		t[0x80+i] = r
	}
	return &t
}()

// singleByte returns the table for the single-byte charsets feeds still use, or nil.
func singleByte(label string) *[256]rune {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "l1", "iso_8859-1":
		return windows1252
	case "windows-1252", "cp1252", "x-cp1252":
		return windows1252
	}
	return nil
}

type decoder struct {
	src   io.Reader
	table *[256]rune
	buf   []byte
}

func (d *decoder) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		in := make([]byte, max(len(p)/3, 1))
		n, err := d.src.Read(in)
		for _, b := range in[:n] {
			d.buf = utf8.AppendRune(d.buf, d.table[b])
		}
		if n == 0 && err != nil {
			return 0, err
		}
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}
