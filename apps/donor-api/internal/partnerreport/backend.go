package partnerreport

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	wpdf "github.com/benoitkugler/go-weasyprint/pdf"
	"github.com/benoitkugler/pdf/model"
	"github.com/benoitkugler/webrender/backend"
	"github.com/benoitkugler/webrender/text"
	sfnt "github.com/tdewolff/font"
)

type fontKey struct {
	origin      text.FontOrigin
	description backend.FontDescription
}

// A glyph can represent distinct source clusters; separate CIDs preserve their text.
type glyphKey struct {
	glyph uint16
	text  string
}
type codedFont struct {
	backend.Font
	name     string
	source   *sfnt.SFNT
	raw, pdf *backend.FontChars
	codes    map[glyphKey]backend.GID
	glyphs   []uint16
}

func (f *codedFont) Description() backend.FontDescription {
	description := f.Font.Description()
	description.Family = f.name
	description.IsOpentype = false
	description.IsOpentypeOpentype = false
	return description
}

type semanticOutput struct {
	*wpdf.Output
	fonts   map[fontKey]*codedFont
	names   map[string]*codedFont
	sources map[text.FontOrigin]*sfnt.SFNT
	err     error
}
type semanticPage struct {
	backend.Page
	output *semanticOutput
}

func (out *semanticOutput) AddPage(left, top, right, bottom backend.Fl) backend.Page {
	return &semanticPage{Page: out.Output.AddPage(left, top, right, bottom), output: out}
}
func (page *semanticPage) AddFont(original backend.Font, content []byte) *backend.FontChars {
	out := page.output
	if out.fonts == nil {
		out.fonts = map[fontKey]*codedFont{}
		out.names = map[string]*codedFont{}
		out.sources = map[text.FontOrigin]*sfnt.SFNT{}
	}
	key := fontKey{original.Origin(), original.Description()}
	if previous := out.fonts[key]; previous != nil {
		return previous.raw
	}
	source := out.sources[original.Origin()]
	if source == nil {
		var err error
		source, err = sfnt.ParseSFNT(content, 0)
		if err != nil {
			out.err = err
			return &backend.FontChars{Cmap: map[backend.GID][]rune{}, Extents: map[backend.GID]backend.GlyphExtents{}}
		}
		out.sources[original.Origin()] = source
	}
	f := &codedFont{Font: original, name: fmt.Sprintf("DADReportFont%d-%s", len(out.fonts), original.Description().Family), source: source,
		raw: &backend.FontChars{
			Cmap:    map[backend.GID][]rune{},
			Extents: map[backend.GID]backend.GlyphExtents{},
		}, codes: map[glyphKey]backend.GID{}, glyphs: []uint16{0}}
	f.pdf = page.Page.AddFont(f, content)
	out.fonts[key], out.names[f.name] = f, f
	return f.raw
}
func (page *semanticPage) DrawText(drawings []backend.TextDrawing) {
	out := page.output
	if out.err != nil {
		return
	}
	copied := append([]backend.TextDrawing(nil), drawings...)
	for i, drawing := range drawings {
		claimed := make([]bool, len(drawing.Text))
		known := make([]bool, len(drawing.Text))
		for _, run := range drawing.Runs {
			for _, glyph := range run.Glyphs {
				for j := glyph.TextOffset; j < glyph.TextOffset+glyph.TextLength && j < len(known); j++ {
					known[j] = true
				}
			}
		}
		var missing []int
		for j, mapped := range known {
			if !mapped {
				missing = append(missing, j)
			}
		}
		copied[i].Runs = append([]backend.TextRun(nil), drawing.Runs...)
		for j, run := range drawing.Runs {
			f := out.fonts[fontKey{run.Font.Origin(), run.Font.Description()}]
			if f == nil {
				out.err = fmt.Errorf("unregistered font")
				return
			}
			copied[i].Runs[j].Font = f
			copied[i].Runs[j].Glyphs = append([]backend.TextGlyph(nil), run.Glyphs...)
			f.pdf.Bbox = f.raw.Bbox
			for k, glyph := range run.Glyphs {
				var sourceText []rune
				original := uint16(glyph.Glyph)
				if glyph.Glyph >= backend.GID(f.source.Maxp.NumGlyphs) {
					original = 0
					if len(missing) > 0 {
						sourceText = []rune{drawing.Text[missing[0]]}
						claimed[missing[0]] = true
						missing = missing[1:]
					}
				} else {
					for pos := glyph.TextOffset; pos < glyph.TextOffset+glyph.TextLength && pos < len(drawing.Text); pos++ {
						if !claimed[pos] {
							sourceText = append(sourceText, drawing.Text[pos])
							claimed[pos] = true
						}
					}
				}
				key := glyphKey{original, string(sourceText)}
				code, found := f.codes[key]
				if !found {
					if len(f.glyphs) > 65535 {
						out.err = ErrTooLarge
						return
					}
					code = backend.GID(len(f.glyphs))
					f.codes[key] = code
					f.glyphs = append(f.glyphs, original)
					extent := f.raw.Extents[glyph.Glyph]
					// Natural advances prevent PDF readers from inventing word spaces.
					extent.Width = int(math.Round(float64(f.source.GlyphAdvance(original)) * 1000 / float64(f.source.Head.UnitsPerEm)))
					f.pdf.Extents[code] = extent
					f.pdf.Cmap[code] = sourceText
				}
				copied[i].Runs[j].Glyphs[k].Glyph = code
				copied[i].Runs[j].Glyphs[k].Kerning += f.pdf.Extents[code].Width - f.raw.Extents[glyph.Glyph].Width
			}
		}
	}
	page.Page.DrawText(copied)
}
func (out *semanticOutput) enhance(ctx context.Context, doc *model.Document) error {
	if out.err != nil {
		return out.err
	}
	seen := map[*model.FontDict]bool{}
	for _, page := range doc.Catalog.Pages.Flatten() {
		for _, font := range page.Resources.Font {
			if err := ctx.Err(); err != nil {
				return err
			}
			if seen[font] {
				continue
			}
			seen[font] = true
			kind, ok := font.Subtype.(model.FontType0)
			if !ok {
				return fmt.Errorf("unexpected font subtype")
			}
			source := out.names[kind.DescendantFonts.FontDescriptor.FontFamily]
			if source == nil {
				return fmt.Errorf("unrecognized font")
			}
			glyphs := []uint16{0}
			subsetIDs := map[uint16]uint16{0: 0}
			for _, glyph := range source.glyphs {
				if _, found := subsetIDs[glyph]; !found {
					subsetIDs[glyph] = uint16(len(glyphs))
					glyphs = append(glyphs, glyph)
				}
			}
			// Subset after shaping, keeping CID mappings independent of source glyph IDs.
			subset, err := source.source.Subset(glyphs, sfnt.SubsetOptions{Tables: sfnt.KeepPDFTables})
			if err != nil {
				return err
			}
			data := subset.Write()
			file := kind.DescendantFonts.FontDescriptor.FontFile
			file.Stream = model.NewCompressedStream(data)
			file.Length1 = len(data)
			var mapping []byte
			for _, glyph := range source.glyphs {
				mapping = binary.BigEndian.AppendUint16(mapping, subsetIDs[glyph])
			}
			kind.DescendantFonts.CIDToGIDMap = model.CIDToGIDMapStream{Stream: model.NewCompressedStream(mapping)}
			font.Subtype = kind
		}
	}
	return nil
}
