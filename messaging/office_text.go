package messaging

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	docxMaxXMLBytes     = 32 << 20
	xlsxMaxXMLBytes     = 16 << 20
	officeMaxZipEntries = 10000
	xlsxMaxSheets       = 100
	xlsxMaxSharedItems  = 200000
	xlsxMaxSharedRunes  = 2000000
	xlsxMaxCellRunes    = 4000
)

type incomingExtraction struct {
	Content         string
	Truncated       bool
	Kind            string
	EncodingProblem bool
	Err             error
}

// extractIncomingContent reads plain text and modern Word/Excel bodies. PDF is
// intentionally not supported.
func extractIncomingContent(name string, data []byte) incomingExtraction {
	ext := strings.ToLower(path.Ext(strings.ReplaceAll(name, "\\", "/")))
	switch ext {
	case ".docx", ".docm":
		text, truncated, err := extractDocxText(data)
		return incomingExtraction{Content: text, Truncated: truncated, Kind: "word", Err: err}
	case ".xlsx", ".xlsm":
		text, truncated, err := extractXlsxText(data)
		return incomingExtraction{Content: text, Truncated: truncated, Kind: "excel", Err: err}
	}

	if !isTextFile(name, data) {
		return incomingExtraction{}
	}
	if !utf8.Valid(data) {
		return incomingExtraction{EncodingProblem: true}
	}
	text, truncated := decodeIncomingText(data)
	return incomingExtraction{Content: text, Truncated: truncated}
}

func officeReadError(kind string) string {
	switch kind {
	case "word":
		return "Word 正文没读出来，可能文件损坏或加密；用 Office/WPS 另存为新的 .docx 再发一次"
	case "excel":
		return "Excel 内容没读出来，可能文件损坏或加密；另存为新的 .xlsx 再发一次"
	default:
		return "文件正文没读出来，可能文件损坏或加密"
	}
}

func officeKindLabel(kind string) string {
	switch kind {
	case "word":
		return "Word 正文"
	case "excel":
		return "Excel 内容"
	default:
		return "正文"
	}
}

func officeKindName(kind string) string {
	switch kind {
	case "word":
		return "Word 文档"
	case "excel":
		return "Excel 表格"
	default:
		return ""
	}
}

func extractDocxText(data []byte) (string, bool, error) {
	entries, err := openOfficeZip(data)
	if err != nil {
		return "", false, err
	}
	document, ok := entries["word/document.xml"]
	if !ok {
		return "", false, fmt.Errorf("docx has no word/document.xml")
	}
	if document.UncompressedSize64 > docxMaxXMLBytes {
		return "", false, fmt.Errorf("docx document.xml is too large")
	}
	rc, err := document.Open()
	if err != nil {
		return "", false, err
	}
	defer rc.Close()

	dec := xml.NewDecoder(io.LimitReader(rc, docxMaxXMLBytes+1))
	dec.Strict = false
	var out incomingTextBuilder
	out.init(incomingTextMaxChars)
	inText := false
	tableDepth := 0

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, fmt.Errorf("parse docx: %w", err)
		}
		switch node := tok.(type) {
		case xml.StartElement:
			switch node.Name.Local {
			case "t":
				inText = true
			case "tab":
				out.add("\t")
			case "br", "cr":
				out.add("\n")
			case "tbl":
				tableDepth++
			}
		case xml.EndElement:
			switch node.Name.Local {
			case "t":
				inText = false
			case "p":
				if tableDepth > 0 {
					out.add(" ")
				} else {
					out.add("\n")
				}
			case "tc":
				out.add("\t")
			case "tr":
				out.add("\n")
			case "tbl":
				if tableDepth > 0 {
					tableDepth--
				}
				if tableDepth == 0 {
					out.add("\n")
				}
			}
		case xml.CharData:
			if inText {
				out.add(string(node))
			}
		}
		if out.truncated {
			break
		}
	}

	text := cleanExtractedDocumentText(out.String())
	if text == "" {
		return "", false, nil
	}
	return text, out.truncated, nil
}

func extractXlsxText(data []byte) (string, bool, error) {
	entries, err := openOfficeZip(data)
	if err != nil {
		return "", false, err
	}
	shared, sharedTruncated, err := readXlsxSharedStrings(entries["xl/sharedStrings.xml"])
	if err != nil {
		return "", false, err
	}
	sheets := readXlsxSheets(entries)
	if len(sheets) == 0 {
		return "", false, fmt.Errorf("xlsx has no worksheets")
	}

	var out incomingTextBuilder
	out.init(incomingTextMaxChars)
	if sharedTruncated {
		out.truncated = true
	}
	for i, sheet := range sheets {
		if i >= xlsxMaxSheets {
			out.truncated = true
			break
		}
		file := entries[sheet.Target]
		if file == nil {
			continue
		}
		if file.UncompressedSize64 > xlsxMaxXMLBytes {
			return "", false, fmt.Errorf("xlsx sheet %q is too large", sheet.Name)
		}
		name := strings.TrimSpace(sheet.Name)
		if name == "" {
			name = fmt.Sprintf("Sheet%d", i+1)
		}
		out.add("\n【" + name + "】\n")
		if err := readXlsxSheet(file, shared, &out); err != nil {
			return "", false, err
		}
		if out.truncated {
			break
		}
	}

	text := cleanExtractedDocumentText(out.String())
	if text == "" {
		return "", false, nil
	}
	return text, out.truncated, nil
}

func openOfficeZip(data []byte) (map[string]*zip.File, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open document: %w", err)
	}
	if len(zr.File) > officeMaxZipEntries {
		return nil, fmt.Errorf("document has too many zip entries")
	}
	entries := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		name := strings.TrimPrefix(path.Clean(strings.ReplaceAll(f.Name, "\\", "/")), "/")
		if name == "" || name == "." || strings.HasSuffix(name, "/") {
			continue
		}
		entries[name] = f
	}
	return entries, nil
}

func readXlsxSharedStrings(file *zip.File) ([]string, bool, error) {
	if file == nil {
		return nil, false, nil
	}
	if file.UncompressedSize64 > xlsxMaxXMLBytes {
		return nil, true, nil
	}
	rc, err := file.Open()
	if err != nil {
		return nil, false, err
	}
	defer rc.Close()

	dec := xml.NewDecoder(io.LimitReader(rc, xlsxMaxXMLBytes+1))
	dec.Strict = false
	var shared []string
	var current strings.Builder
	totalRunes := 0
	truncated := false
	inSI, inText, inPhonetic := false, false, false

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("parse xlsx shared strings: %w", err)
		}
		switch node := tok.(type) {
		case xml.StartElement:
			switch node.Name.Local {
			case "si":
				inSI = true
				current.Reset()
			case "rPh":
				inPhonetic = true
			case "t":
				if inSI && !inPhonetic {
					inText = true
				}
			}
		case xml.EndElement:
			switch node.Name.Local {
			case "t":
				inText = false
			case "rPh":
				inPhonetic = false
			case "si":
				inSI = false
				if len(shared) >= xlsxMaxSharedItems || totalRunes+current.Len() > xlsxMaxSharedRunes {
					truncated = true
					continue
				}
				value := current.String()
				shared = append(shared, value)
				totalRunes += utf8.RuneCountInString(value)
			}
		case xml.CharData:
			if inText {
				current.WriteString(string(node))
			}
		}
	}
	return shared, truncated, nil
}

type xlsxSheet struct {
	Name   string
	Target string
}

func readXlsxSheets(entries map[string]*zip.File) []xlsxSheet {
	rels := make(map[string]string)
	if relFile := entries["xl/_rels/workbook.xml.rels"]; relFile != nil {
		if rc, err := relFile.Open(); err == nil {
			dec := xml.NewDecoder(io.LimitReader(rc, xlsxMaxXMLBytes+1))
			dec.Strict = false
			for {
				tok, err := dec.Token()
				if err != nil {
					break
				}
				if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "Relationship" {
					id := attrByLocal(start, "Id")
					target := attrByLocal(start, "Target")
					relType := attrByLocal(start, "Type")
					if id != "" && target != "" && (relType == "" || strings.HasSuffix(relType, "/worksheet")) {
						rels[id] = resolveXlsxTarget(target)
					}
				}
			}
			_ = rc.Close()
		}
	}

	var sheets []xlsxSheet
	if wb := entries["xl/workbook.xml"]; wb != nil {
		if rc, err := wb.Open(); err == nil {
			dec := xml.NewDecoder(io.LimitReader(rc, xlsxMaxXMLBytes+1))
			dec.Strict = false
			for {
				tok, err := dec.Token()
				if err != nil {
					break
				}
				if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "sheet" {
					name := attrByLocal(start, "name")
					rid := attrByLocal(start, "id")
					if target := rels[rid]; target != "" {
						sheets = append(sheets, xlsxSheet{Name: name, Target: target})
					}
				}
			}
			_ = rc.Close()
		}
	}
	if len(sheets) > 0 {
		return sheets
	}

	// Some generators omit workbook relationships; fall back to the physical
	// worksheet order so the data is still readable.
	for name := range entries {
		if strings.HasPrefix(name, "xl/worksheets/sheet") && strings.HasSuffix(name, ".xml") {
			sheets = append(sheets, xlsxSheet{Target: name})
		}
	}
	sort.Slice(sheets, func(i, j int) bool { return sheets[i].Target < sheets[j].Target })
	for i := range sheets {
		sheets[i].Name = fmt.Sprintf("Sheet%d", i+1)
	}
	return sheets
}

func resolveXlsxTarget(target string) string {
	target = strings.ReplaceAll(strings.TrimSpace(target), "\\", "/")
	if strings.HasPrefix(target, "/") {
		return path.Clean(strings.TrimPrefix(target, "/"))
	}
	cleaned := path.Clean(path.Join("xl", target))
	if !strings.HasPrefix(cleaned, "xl/") {
		return ""
	}
	return cleaned
}

func attrByLocal(start xml.StartElement, local string) string {
	for _, attr := range start.Attr {
		if attr.Name.Local == local {
			return attr.Value
		}
	}
	return ""
}

func readXlsxSheet(file *zip.File, shared []string, out *incomingTextBuilder) error {
	rc, err := file.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := xml.NewDecoder(io.LimitReader(rc, xlsxMaxXMLBytes+1))
	dec.Strict = false
	var cell strings.Builder
	var row []string
	cellType := ""
	inCell, inValue, inInlineText := false, false, false

	flushRow := func() {
		for len(row) > 0 && strings.TrimSpace(row[len(row)-1]) == "" {
			row = row[:len(row)-1]
		}
		if len(row) > 0 {
			out.add(strings.Join(row, "\t") + "\n")
		}
		row = row[:0]
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("parse xlsx sheet: %w", err)
		}
		switch node := tok.(type) {
		case xml.StartElement:
			switch node.Name.Local {
			case "c":
				inCell = true
				cellType = attrByLocal(node, "t")
				cell.Reset()
			case "v":
				if inCell {
					inValue = true
				}
			case "t":
				if inCell && cellType == "inlineStr" {
					inInlineText = true
				}
			}
		case xml.EndElement:
			switch node.Name.Local {
			case "v":
				inValue = false
			case "t":
				inInlineText = false
			case "c":
				value := cell.String()
				switch cellType {
				case "s":
					if idx, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && idx >= 0 && idx < len(shared) {
						value = shared[idx]
					} else {
						value = ""
					}
				case "b":
					if strings.TrimSpace(value) == "1" {
						value = "TRUE"
					} else if strings.TrimSpace(value) == "0" {
						value = "FALSE"
					}
				}
				row = append(row, cleanXlsxCell(value))
				inCell = false
				inValue = false
				inInlineText = false
				cell.Reset()
				cellType = ""
			case "row":
				flushRow()
				if out.truncated {
					return nil
				}
			}
		case xml.CharData:
			if inValue || inInlineText {
				if cell.Len() < 64<<10 {
					cell.WriteString(string(node))
				}
			}
		}
	}
	flushRow()
	return nil
}

func cleanXlsxCell(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > xlsxMaxCellRunes {
		return string(runes[:xlsxMaxCellRunes]) + "…"
	}
	return value
}

func cleanExtractedDocumentText(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = strings.ReplaceAll(raw, "\u00a0", " ")
	raw = strings.ReplaceAll(raw, "\u200b", "")

	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		for strings.Contains(line, " \t") {
			line = strings.ReplaceAll(line, " \t", "\t")
		}
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			if len(out) > 0 && !blank {
				out = append(out, "")
				blank = true
			}
			continue
		}
		out = append(out, line)
		blank = false
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

type incomingTextBuilder struct {
	sb        strings.Builder
	runes     int
	max       int
	truncated bool
}

func (b *incomingTextBuilder) init(max int) {
	b.max = max
	b.sb.Grow(min(max, 4096))
}

func (b *incomingTextBuilder) add(s string) {
	if s == "" || b.truncated {
		return
	}
	if b.runes >= b.max {
		b.truncated = true
		return
	}
	remaining := b.max - b.runes
	runes := []rune(s)
	if len(runes) > remaining {
		b.sb.WriteString(string(runes[:remaining]))
		b.runes = b.max
		b.truncated = true
		return
	}
	b.sb.WriteString(s)
	b.runes += len(runes)
}

func (b *incomingTextBuilder) String() string {
	return b.sb.String()
}
