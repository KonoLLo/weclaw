package messaging

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestParseExportCommand(t *testing.T) {
	cases := []struct {
		in   string
		kind exportKind
		body string
		ok   bool
	}{
		{"/word 报告", exportWord, "报告", true},
		{"/word", exportWord, "", true},
		{"/excel a | b", exportExcel, "a | b", true},
		{"/导出 word 你好", exportWord, "你好", true},
		{"/导出 excel x", exportExcel, "x", true},
		{"/wordpress", 0, "", false},
		{"/other", 0, "", false},
	}
	for _, c := range cases {
		kind, body, ok := parseExportCommand(c.in)
		if kind != c.kind || body != c.body || ok != c.ok {
			t.Errorf("parseExportCommand(%q) = (%v,%q,%v), want (%v,%q,%v)", c.in, kind, body, ok, c.kind, c.body, c.ok)
		}
	}
}

func TestSheetRows(t *testing.T) {
	rows := sheetRows("| 名称 | 数量 |\n| --- | ---: |\n| 苹果 | 3 |")
	if len(rows) != 2 || rows[0][0] != "名称" || rows[1][1] != "3" {
		t.Fatalf("pipe rows = %#v", rows)
	}
	rows = sheetRows("a\tb\nc\td")
	if len(rows) != 2 || rows[1][1] != "d" {
		t.Fatalf("tab rows = %#v", rows)
	}
	rows = sheetRows("第一行\n第二行")
	if len(rows) != 2 || rows[0][0] != "第一行" {
		t.Fatalf("line rows = %#v", rows)
	}
}

func TestBuildDOCXAndXLSX(t *testing.T) {
	docx, err := buildDOCX("# 标题\n\n正文\n\n| 名称 | 数量 |\n| --- | --- |\n| 苹果 | 3 |")
	if err != nil {
		t.Fatal(err)
	}
	parts := readZipParts(t, docx)
	assertWellFormedXML(t, parts)
	doc := parts["word/document.xml"]
	for _, want := range []string{"标题", "正文", "<w:tbl>", "<w:gridCol", "苹果"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("docx document missing %q: %s", want, doc)
		}
	}
	if _, ok := parts["word/styles.xml"]; !ok {
		t.Fatal("docx missing styles.xml")
	}

	xlsx, err := buildXLSX("| 名称 | 数量 |\n| --- | --- |\n| 苹果 | 3 |")
	if err != nil {
		t.Fatal(err)
	}
	parts = readZipParts(t, xlsx)
	assertWellFormedXML(t, parts)
	sheet := parts["xl/worksheets/sheet1.xml"]
	for _, want := range []string{`r="A1"`, `r="B1"`, "苹果", "inlineStr"} {
		if !strings.Contains(sheet, want) {
			t.Fatalf("xlsx sheet missing %q: %s", want, sheet)
		}
	}
	if _, ok := parts["xl/styles.xml"]; !ok {
		t.Fatal("xlsx missing styles.xml")
	}
}

func assertWellFormedXML(t *testing.T, parts map[string]string) {
	t.Helper()
	for name, body := range parts {
		if !strings.HasSuffix(name, ".xml") && !strings.HasSuffix(name, ".rels") {
			continue
		}
		dec := xml.NewDecoder(strings.NewReader(body))
		for {
			if _, err := dec.Token(); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("%s is not well-formed XML: %v\n%s", name, err, body)
			}
		}
	}
}

func readZipParts(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(body)
	}
	return out
}
