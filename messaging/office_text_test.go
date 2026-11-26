package messaging

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func buildTestZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractDocxText(t *testing.T) {
	document := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>
<w:p><w:r><w:t>第一段</w:t></w:r></w:p>
<w:p><w:r><w:t xml:space="preserve">Hello</w:t></w:r><w:r><w:t> world</w:t></w:r></w:p>
<w:tbl>
<w:tr><w:tc><w:p><w:r><w:t>姓名</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>年龄</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:p><w:r><w:t>张三</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>30</w:t></w:r></w:p></w:tc></w:tr>
</w:tbl>
<w:p><w:r><w:delText>删除的内容</w:delText><w:t>保留的内容</w:t></w:r></w:p>
<w:p><w:r><w:t>A</w:t><w:tab/><w:t>B</w:t></w:r></w:p>
</w:body>
</w:document>`
	data := buildTestZip(t, map[string]string{"word/document.xml": document})

	ex := extractIncomingContent("会议笔记.docx", data)
	if ex.Err != nil {
		t.Fatalf("extract: %v", ex.Err)
	}
	if ex.Kind != "word" {
		t.Fatalf("kind=%q, want word", ex.Kind)
	}
	for _, want := range []string{"第一段", "Hello world", "姓名\t年龄", "张三\t30", "保留的内容", "A\tB"} {
		if !strings.Contains(ex.Content, want) {
			t.Fatalf("missing %q in extracted text:\n%s", want, ex.Content)
		}
	}
	if strings.Contains(ex.Content, "删除的内容") {
		t.Fatalf("deleted Word text leaked:\n%s", ex.Content)
	}
}

func TestExtractDocxTruncates(t *testing.T) {
	document := `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` +
		strings.Repeat("长", incomingTextMaxChars+100) +
		`</w:t></w:r></w:p></w:body></w:document>`
	data := buildTestZip(t, map[string]string{"word/document.xml": document})

	ex := extractIncomingContent("长文.docx", data)
	if ex.Err != nil {
		t.Fatalf("extract: %v", ex.Err)
	}
	if !ex.Truncated {
		t.Fatal("long docx was not marked truncated")
	}
	if n := len([]rune(ex.Content)); n > incomingTextMaxChars {
		t.Fatalf("extracted %d runes, want <= %d", n, incomingTextMaxChars)
	}
}

func TestExtractDocxBroken(t *testing.T) {
	ex := extractIncomingContent("坏文件.docx", []byte("not a zip"))
	if ex.Err == nil {
		t.Fatal("broken docx should return an error")
	}
	if ex.Kind != "word" {
		t.Fatalf("kind=%q, want word", ex.Kind)
	}
	if !strings.Contains(officeReadError(ex.Kind), "Word") {
		t.Fatalf("unexpected friendly error: %q", officeReadError(ex.Kind))
	}
}

func TestExtractXlsxText(t *testing.T) {
	files := map[string]string{
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets><sheet name="销售" sheetId="1" r:id="rId1"/><sheet name="汇总" sheetId="2" r:id="rId2"/></sheets>
</workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet2.xml"/>
</Relationships>`,
		"xl/sharedStrings.xml": `<?xml version="1.0" encoding="UTF-8"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<si><t>产品</t></si>
<si><r><t>销</t></r><r><t>量</t></r></si>
<si><t>苹果</t></si>
<si><rPh><t>ignored</t></rPh><t>香蕉</t></si>
</sst>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>
<row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><v>42</v></c><c r="C2" t="b"><v>1</v></c></row>
<row r="3"><c r="A3" t="inlineStr"><is><t>合计</t></is></c><c r="B3"><f>SUM(B2:B2)</f><v>42</v></c></row>
</sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>
<row r="1"><c r="A1" t="s"><v>3</v></c></row>
</sheetData></worksheet>`,
	}
	data := buildTestZip(t, files)
	ex := extractIncomingContent("销售表.xlsx", data)
	if ex.Err != nil {
		t.Fatalf("extract: %v", ex.Err)
	}
	if ex.Kind != "excel" {
		t.Fatalf("kind=%q, want excel", ex.Kind)
	}
	for _, want := range []string{"【销售】", "产品\t销量", "苹果\t42\tTRUE", "合计\t42", "【汇总】", "香蕉"} {
		if !strings.Contains(ex.Content, want) {
			t.Fatalf("missing %q in extracted text:\n%s", want, ex.Content)
		}
	}
	if strings.Contains(ex.Content, "ignored") {
		t.Fatalf("phonetic text leaked:\n%s", ex.Content)
	}
}

func TestExtractXlsxFallbackSheetOrder(t *testing.T) {
	files := map[string]string{
		"xl/worksheets/sheet2.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row><c><v>2</v></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet1.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row><c><v>1</v></c></row></sheetData></worksheet>`,
	}
	ex := extractIncomingContent("无关系表.xlsx", buildTestZip(t, files))
	if ex.Err != nil {
		t.Fatalf("extract: %v", ex.Err)
	}
	if !strings.Contains(ex.Content, "【Sheet1】") || !strings.Contains(ex.Content, "【Sheet2】") {
		t.Fatalf("fallback sheets missing:\n%s", ex.Content)
	}
	if strings.Index(ex.Content, "【Sheet1】") > strings.Index(ex.Content, "【Sheet2】") {
		t.Fatalf("fallback sheets out of order:\n%s", ex.Content)
	}
}

func TestExtractIncomingContentTextAndUnsupported(t *testing.T) {
	ex := extractIncomingContent("notes.txt", []byte("hello\nworld"))
	if ex.Err != nil || ex.Kind != "" || ex.Content != "hello\nworld" {
		t.Fatalf("unexpected text extraction: %+v", ex)
	}
	ex = extractIncomingContent("manual.pdf", []byte("%PDF-1.7 trailer"))
	if ex.Err != nil || ex.Kind != "" || ex.Content != "" {
		t.Fatalf("pdf should be unsupported without error: %+v", ex)
	}
	ex = extractIncomingContent("legacy.doc", []byte{0xd0, 0xcf, 0x11, 0xe0})
	if ex.Err != nil || ex.Kind != "" || ex.Content != "" {
		t.Fatalf("legacy doc should be unsupported without error: %+v", ex)
	}
}

func TestOfficePromptLabels(t *testing.T) {
	word := pendingIncomingFile{
		Name:    "报告.docx",
		Path:    "/tmp/报告.docx",
		Content: "报告正文",
		Kind:    "word",
	}
	got := formatIncomingFilePrompt(word, "http", "帮我总结")
	if !strings.Contains(got, "（Word 文档）") || !strings.Contains(got, "报告正文") {
		t.Fatalf("word prompt missing label/body: %q", got)
	}

	broken := pendingIncomingFile{
		Name:    "坏文件.xlsx",
		Path:    "/tmp/坏文件.xlsx",
		Kind:    "excel",
		ReadErr: officeReadError("excel"),
	}
	got = formatIncomingFilePrompt(broken, "http", "看看这个表格")
	if !strings.Contains(got, "Excel 内容没读出来") {
		t.Fatalf("broken excel prompt missing reason: %q", got)
	}
}

func TestExtractXlsxBroken(t *testing.T) {
	ex := extractIncomingContent("坏表.xlsx", []byte("not a zip"))
	if ex.Err == nil || ex.Kind != "excel" {
		t.Fatalf("broken xlsx: %+v", ex)
	}
}
