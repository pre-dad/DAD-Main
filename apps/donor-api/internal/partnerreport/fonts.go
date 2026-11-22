package partnerreport

import "unicode"

type fontSpec struct {
	file, family string
	weight       int
	scripts      []*unicode.RangeTable
	// always selects the font for every report; match selects it for runes
	// the scripts do not cover.
	always bool
	match  func(rune) bool
}

// isCJKPunctuation covers the CJK radicals, symbols and punctuation blocks.
func isCJKPunctuation(r rune) bool { return r >= 0x2e80 && r <= 0x303f }

// isSymbol reports runes outside Latin-1 that the Latin face lacks: symbols,
// math operators and mathematical alphanumerics.
func isSymbol(r rune) bool {
	return r > 0xff && (unicode.Is(unicode.So, r) || unicode.Is(unicode.Sm, r) || (r >= 0x1d400 && r <= 0x1d7ff))
}

var fontCatalog = []fontSpec{
	{file: "NotoSans-Regular.ttf", family: "ReportLatin", weight: 400, always: true},
	{file: "NotoSans-Bold.ttf", family: "ReportLatin", weight: 700, always: true},
	{
		file:    "NotoSansCJKjp-Regular.ttf",
		family:  "ReportCJK",
		weight:  400,
		scripts: []*unicode.RangeTable{unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana, unicode.Bopomofo},
		match:   isCJKPunctuation,
	},
	{file: "NotoSansArabic-Regular.ttf", family: "ReportArabic", weight: 400, scripts: []*unicode.RangeTable{unicode.Arabic}},
	{file: "NotoSansHebrew-Regular.ttf", family: "ReportHebrew", weight: 400, scripts: []*unicode.RangeTable{unicode.Hebrew}},
	{file: "NotoSansDevanagari-Regular.ttf", family: "ReportDevanagari", weight: 400, scripts: []*unicode.RangeTable{unicode.Devanagari}},
	{file: "NotoSansBengali-Regular.ttf", family: "ReportBengali", weight: 400, scripts: []*unicode.RangeTable{unicode.Bengali}},
	{file: "NotoSansTamil-Regular.ttf", family: "ReportTamil", weight: 400, scripts: []*unicode.RangeTable{unicode.Tamil}},
	{file: "NotoSansTelugu-Regular.ttf", family: "ReportTelugu", weight: 400, scripts: []*unicode.RangeTable{unicode.Telugu}},
	{file: "NotoSansKannada-Regular.ttf", family: "ReportKannada", weight: 400, scripts: []*unicode.RangeTable{unicode.Kannada}},
	{file: "NotoSansMalayalam-Regular.ttf", family: "ReportMalayalam", weight: 400, scripts: []*unicode.RangeTable{unicode.Malayalam}},
	{file: "NotoSansGujarati-Regular.ttf", family: "ReportGujarati", weight: 400, scripts: []*unicode.RangeTable{unicode.Gujarati}},
	{file: "NotoSansGurmukhi-Regular.ttf", family: "ReportGurmukhi", weight: 400, scripts: []*unicode.RangeTable{unicode.Gurmukhi}},
	{file: "NotoSansSinhala-Regular.ttf", family: "ReportSinhala", weight: 400, scripts: []*unicode.RangeTable{unicode.Sinhala}},
	{file: "NotoSansThai-Regular.ttf", family: "ReportThai", weight: 400, scripts: []*unicode.RangeTable{unicode.Thai}},
	{file: "NotoSansLao-Regular.ttf", family: "ReportLao", weight: 400, scripts: []*unicode.RangeTable{unicode.Lao}},
	{file: "NotoSansKhmer-Regular.ttf", family: "ReportKhmer", weight: 400, scripts: []*unicode.RangeTable{unicode.Khmer}},
	{file: "NotoSansMyanmar-Regular.ttf", family: "ReportMyanmar", weight: 400, scripts: []*unicode.RangeTable{unicode.Myanmar}},
	{file: "NotoSansEthiopic-Regular.ttf", family: "ReportEthiopic", weight: 400, scripts: []*unicode.RangeTable{unicode.Ethiopic}},
	{file: "NotoSansGeorgian-Regular.ttf", family: "ReportGeorgian", weight: 400, scripts: []*unicode.RangeTable{unicode.Georgian}},
	{file: "NotoSansArmenian-Regular.ttf", family: "ReportArmenian", weight: 400, scripts: []*unicode.RangeTable{unicode.Armenian}},
	{file: "NotoEmoji-Regular.ttf", family: "ReportEmoji", weight: 400, match: isSymbol},
	{file: "NotoSansSymbols2-Regular.ttf", family: "ReportSymbols", weight: 400, match: isSymbol},
	{file: "NotoSansMath-Regular.ttf", family: "ReportMath", weight: 400, match: isSymbol},
}

func selectFonts(data Data) []fontSpec {
	needed := make([]bool, len(fontCatalog))
	for i, font := range fontCatalog {
		needed[i] = font.always
	}
	inspect := func(value string) {
		for _, r := range value {
			for i, font := range fontCatalog {
				if needed[i] {
					continue
				}
				needed[i] = font.match != nil && font.match(r)
				for _, script := range font.scripts {
					if unicode.Is(script, r) {
						needed[i] = true
						break
					}
				}
			}
		}
	}
	inspect(data.Organization)
	for _, row := range data.Spend {
		inspect(row.Category)
	}
	for _, metric := range data.Metrics {
		inspect(metric.Label)
		inspect(metric.Value)
	}
	selected := make([]fontSpec, 0, len(fontCatalog))
	for i, font := range fontCatalog {
		if needed[i] {
			selected = append(selected, font)
		}
	}
	return selected
}
