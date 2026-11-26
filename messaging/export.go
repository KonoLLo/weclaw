package messaging

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fastclaw-ai/weclaw/ilink"
)

const (
	exportMaxRunes = 60000
	exportMaxRows  = 5000
	exportMaxCols  = 60
)

type exportKind int

const (
	exportWord exportKind = iota
	exportExcel
)

func parseExportCommand(trimmed string) (exportKind, string, bool) {
	text := strings.TrimSpace(trimmed)
	lower := strings.ToLower(text)
	if rest, ok := cutCommand(text, lower, "/word"); ok {
		return exportWord, rest, true
	}
	if rest, ok := cutCommand(text, lower, "/excel"); ok {
		return exportExcel, rest, true
	}
	if rest, ok := cutCommand(text, lower, "/导出"); ok {
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, "", false
		}
		sub := strings.ToLower(fields[0])
		body := strings.TrimSpace(strings.TrimPrefix(rest, fields[0]))
		switch sub {
		case "word", "文档":
			return exportWord, body, true
		case "excel", "表格":
			return exportExcel, body, true
		}
	}
	return 0, "", false
}

func cutCommand(text, lower, prefix string) (string, bool) {
	if !strings.HasPrefix(lower, prefix) {
		return "", false
	}
	if len(text) > len(prefix) {
		next := text[len(prefix)]
		if next != ' ' && next != '\t' && next != '\r' && next != '\n' {
			return "", false
		}
	}
	return strings.TrimSpace(text[len(prefix):]), true
}

func (h *Handler) handleExportCommand(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, trimmed, clientID string) bool {
	kind, content, ok := parseExportCommand(trimmed)
	if !ok {
		return false
	}
	label := "Word"
	if kind == exportExcel {
		label = "Excel"
	}
	if strings.TrimSpace(content) == "" {
		p, found := h.currentPendingIncomingFile(msg.FromUserID)
		if !found {
			h.sendWorkspaceText(ctx, client, msg, fmt.Sprintf("用法：/%s <内容>\n也可以先发一个 txt/Markdown/Word/Excel 文件，再发 /%s。", strings.ToLower(label), strings.ToLower(label)), clientID)
			return true
		}
		if p.ReadErr != "" {
			h.sendWorkspaceText(ctx, client, msg, "这个文件没读到正文："+p.ReadErr, clientID)
			return true
		}
		if strings.TrimSpace(p.Content) == "" {
			h.sendWorkspaceText(ctx, client, msg, fmt.Sprintf("这个文件没有可导出的文字。可以直接发 /%s <内容>。", strings.ToLower(label)), clientID)
			return true
		}
		content = p.Content
	}
	if runes := []rune(content); len(runes) > exportMaxRunes {
		content = string(runes[:exportMaxRunes])
		log.Printf("[export] content truncated to %d runes for %s", exportMaxRunes, msg.FromUserID)
	}

	path, cleanup, err := writeExportFile(kind, content)
	if err != nil {
		h.sendWorkspaceText(ctx, client, msg, "导出失败："+err.Error(), clientID)
		return true
	}
	defer cleanup()
	if err := SendMediaFromPath(ctx, client, msg.FromUserID, path, msg.ContextToken); err != nil {
		h.sendWorkspaceText(ctx, client, msg, "文件生成好了，但发送失败："+err.Error(), clientID)
		return true
	}
	log.Printf("[export] sent %s export to %s (%s)", label, msg.FromUserID, filepath.Base(path))
	h.sendWorkspaceText(ctx, client, msg, fmt.Sprintf("已导出 %s：%s", label, filepath.Base(path)), clientID)
	return true
}

func writeExportFile(kind exportKind, content string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "weclaw-export-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	stamp := time.Now().Format("20060102-150405")
	var name string
	var data []byte
	if kind == exportExcel {
		name = "表格-" + stamp + ".xlsx"
		data, err = buildXLSX(content)
	} else {
		name = "文档-" + stamp + ".docx"
		data, err = buildDOCX(content)
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

func xmlText(s string) string {
	var clean strings.Builder
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			clean.WriteRune(r)
		case r >= 0x20 && r <= 0xD7FF:
			clean.WriteRune(r)
		case r >= 0xE000 && r <= 0xFFFD:
			clean.WriteRune(r)
		case r >= 0x10000 && r <= 0x10FFFF:
			clean.WriteRune(r)
		default:
			clean.WriteRune(' ')
		}
	}
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(clean.String()))
	return b.String()
}

func addZipPart(zw *zip.Writer, name string, content string) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(content))
	return err
}

func buildDOCX(content string) ([]byte, error) {
	var body strings.Builder
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue
		}
		if cells, ok := tableRow(line); ok && i+1 < len(lines) {
			if next, ok := tableRow(lines[i+1]); ok && isTableSeparator(next) {
				rows := [][]string{cells}
				i += 2
				for ; i < len(lines); i++ {
					row, ok := tableRow(lines[i])
					if !ok {
						i--
						break
					}
					rows = append(rows, row)
				}
				body.WriteString(docxTable(rows))
				continue
			}
		}
		body.WriteString(docxParagraph(line))
	}
	document := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body.String() +
		`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="708" w:footer="708" w:gutter="0"/></w:sectPr></w:body></w:document>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	parts := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/><Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/></Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml": document,
		"word/_rels/document.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`,
		"word/styles.xml": docxStyles,
	}
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml", "word/_rels/document.xml.rels", "word/styles.xml"} {
		if err := addZipPart(zw, name, parts[name]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func docxParagraph(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "<w:p/>"
	}
	style := ""
	text := line
	if level, rest, ok := headingLine(trimmed); ok {
		style = fmt.Sprintf(`<w:pPr><w:pStyle w:val="Heading%d"/></w:pPr>`, level)
		text = rest
	} else if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
		text = "• " + strings.TrimSpace(trimmed[2:])
		style = `<w:pPr><w:ind w:left="360"/></w:pPr>`
	} else if strings.HasPrefix(trimmed, "• ") {
		text = trimmed
		style = `<w:pPr><w:ind w:left="360"/></w:pPr>`
	}
	return `<w:p>` + style + `<w:r><w:t xml:space="preserve">` + xmlText(text) + `</w:t></w:r></w:p>`
}

func headingLine(line string) (int, string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return 0, "", false
	}
	if level > 3 {
		level = 3
	}
	return level, strings.TrimSpace(line[level:]), true
}

func docxTable(rows [][]string) string {
	cols := 1
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	if cols > exportMaxCols {
		cols = exportMaxCols
	}
	var b strings.Builder
	b.WriteString(`<w:tbl><w:tblPr><w:tblStyle w:val="TableGrid"/><w:tblW w:type="auto" w:w="0"/></w:tblPr><w:tblGrid>`)
	for i := 0; i < cols; i++ {
		fmt.Fprintf(&b, `<w:gridCol w:w="%d"/>`, 9000/cols)
	}
	b.WriteString(`</w:tblGrid>`)
	for i, row := range rows {
		if len(row) > cols {
			row = row[:cols]
		}
		b.WriteString("<w:tr>")
		for _, cell := range row {
			b.WriteString(`<w:tc><w:tcPr><w:tcW w:type="auto"/></w:tcPr><w:p><w:r>`)
			if i == 0 {
				b.WriteString("<w:rPr><w:b/></w:rPr>")
			}
			b.WriteString(`<w:t xml:space="preserve">` + xmlText(cell) + `</w:t></w:r></w:p></w:tc>`)
		}
		b.WriteString("</w:tr>")
	}
	b.WriteString("</w:tbl>")
	return b.String()
}

const docxStyles = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Calibri" w:hAnsi="Calibri" w:eastAsia="微软雅黑"/><w:sz w:val="22"/><w:szCs w:val="22"/></w:rPr></w:rPrDefault><w:pPrDefault><w:pPr><w:spacing w:after="120" w:line="360" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults><w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/><w:qFormat/></w:style><w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:spacing w:before="240" w:after="120"/><w:outlineLvl w:val="0"/></w:pPr><w:rPr><w:b/><w:sz w:val="36"/></w:rPr></w:style><w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:spacing w:before="200" w:after="100"/><w:outlineLvl w:val="1"/></w:pPr><w:rPr><w:b/><w:sz w:val="30"/></w:rPr></w:style><w:style w:type="paragraph" w:styleId="Heading3"><w:name w:val="heading 3"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:spacing w:before="160" w:after="80"/><w:outlineLvl w:val="2"/></w:pPr><w:rPr><w:b/><w:sz w:val="26"/></w:rPr></w:style><w:style w:type="table" w:styleId="TableGrid"><w:name w:val="Table Grid"/><w:tblPr><w:tblBorders><w:top w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:left w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:bottom w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:right w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:insideH w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:insideV w:val="single" w:sz="4" w:space="0" w:color="auto"/></w:tblBorders></w:tblPr></w:style></w:styles>`

func tableRow(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, "|") {
		return nil, false
	}
	if strings.HasPrefix(trimmed, "|") {
		trimmed = trimmed[1:]
	}
	if strings.HasSuffix(trimmed, "|") {
		trimmed = trimmed[:len(trimmed)-1]
	}
	parts := strings.Split(trimmed, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, true
}

func isTableSeparator(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		cell = strings.TrimPrefix(cell, ":")
		cell = strings.TrimSuffix(cell, ":")
		if len(cell) < 3 || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

func buildXLSX(content string) ([]byte, error) {
	rows := sheetRows(content)
	if len(rows) == 0 {
		return nil, fmt.Errorf("没有可导出的内容")
	}
	if len(rows) > exportMaxRows {
		rows = rows[:exportMaxRows]
	}
	widths := make([]int, 0, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if i >= exportMaxCols {
				break
			}
			if i >= len(widths) {
				widths = append(widths, 10)
			}
			w := len([]rune(cell)) + 2
			if w > widths[i] {
				widths[i] = w
			}
		}
	}
	for i := range widths {
		if widths[i] > 60 {
			widths[i] = 60
		}
	}

	var sheet strings.Builder
	sheet.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><cols>`)
	for i, width := range widths {
		fmt.Fprintf(&sheet, `<col min="%d" max="%d" width="%d" customWidth="1"/>`, i+1, i+1, width)
	}
	sheet.WriteString("</cols><sheetData>")
	for r, row := range rows {
		fmt.Fprintf(&sheet, `<row r="%d">`, r+1)
		for c, cell := range row {
			if c >= exportMaxCols {
				break
			}
			cell = strings.TrimSpace(cell)
			style := ""
			if r == 0 {
				style = ` s="1"`
			}
			fmt.Fprintf(&sheet, `<c r="%s%d"%s t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, excelColumn(c), r+1, style, xmlText(cell))
		}
		sheet.WriteString("</row>")
	}
	sheet.WriteString("</sheetData></worksheet>")

	workbook := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="数据" sheetId="1" r:id="rId1"/></sheets></workbook>`
	styles := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts><fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills><borders count="1"><border/></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="2"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`

	parts := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/></Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
		"xl/workbook.xml": workbook,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml": sheet.String(),
		"xl/styles.xml":            styles,
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml", "xl/styles.xml"} {
		if err := addZipPart(zw, name, parts[name]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sheetRows(content string) [][]string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	qualified := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue
		}
		qualified = append(qualified, line)
	}
	if len(qualified) == 0 {
		return nil
	}
	mode := "line"
	for _, line := range qualified {
		if strings.Contains(line, "\t") {
			mode = "tab"
			break
		}
	}
	if mode == "line" {
		for _, line := range qualified {
			if strings.Contains(line, "|") {
				mode = "pipe"
				break
			}
		}
	}
	var rows [][]string
	for _, line := range qualified {
		var cells []string
		switch mode {
		case "tab":
			cells = strings.Split(line, "\t")
		case "pipe":
			if parsed, ok := tableRow(line); ok {
				cells = parsed
			} else {
				cells = []string{line}
			}
		default:
			cells = []string{line}
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if mode == "pipe" && isTableSeparator(cells) {
			continue
		}
		rows = append(rows, cells)
	}
	return rows
}

func excelColumn(index int) string {
	name := ""
	for index >= 0 {
		name = string(rune('A'+index%26)) + name
		index = index/26 - 1
	}
	return name
}
