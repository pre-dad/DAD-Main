package partnerreport

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"image/png"
	"io"
	"log"
	"log/slog"
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	wpdf "github.com/benoitkugler/go-weasyprint/pdf"
	"github.com/benoitkugler/webrender/html/document"
	"github.com/benoitkugler/webrender/html/tree"
	"github.com/benoitkugler/webrender/logger"
	"github.com/benoitkugler/webrender/text"
	"github.com/benoitkugler/webrender/utils"
	"github.com/go-text/typesetting/fontscan"
	"golang.org/x/text/unicode/bidi"
)

// Version participates in the immutable snapshot key when rendering changes.
const Version = 2
const MaxSpendRows = 1000
const MaxPDFBytes = 2 << 20

var ErrTooLarge = errors.New("report exceeds the export size limit; existing reports remain available")
var ErrRender = errors.New("report could not be rendered")

//go:embed fonts/*.ttf fonts/OFL-*.txt fonts/provenance.json template.html
var assets embed.FS

var reportTemplate = sync.OnceValues(func() (*template.Template, error) {
	markup, err := assets.ReadFile("template.html")
	if err != nil {
		return nil, err
	}
	return template.New("report").Funcs(template.FuncMap{"direction": textDirection}).Parse(string(markup))
})

// The shaping engine has mutable caches; requests waiting for it honor cancellation.
var renderSlot = make(chan struct{}, 1)

func init() { logger.ProgressLogger.SetOutput(io.Discard) }

type Metric struct{ Label, Value string }
type Spend struct{ Category, Amount string }
type Data struct {
	Organization string
	ExportedAt   time.Time
	UpdatedAt    *time.Time
	Metrics      []Metric
	Spend        []Spend
	Logo         []byte
}

type templateData struct {
	Data
	FontCSS, FontFamilies template.CSS
	Exported, Updated     string
	HasLogo               bool
	LogoWidth, LogoHeight float64
}

func Render(ctx context.Context, data Data) (pdf []byte, err error) {
	if err := validate(data); err != nil {
		return nil, err
	}
	select {
	case renderSlot <- struct{}{}:
		defer func() { <-renderSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "report rendering panicked", "panic_type", fmt.Sprintf("%T", r), "stack", string(debug.Stack()))
			pdf = nil
			err = ErrRender
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	view := templateData{Data: data, Exported: data.ExportedAt.UTC().Format("2 January 2006, 15:04 UTC")}
	if data.UpdatedAt != nil {
		view.Updated = data.UpdatedAt.UTC().Format("2 January 2006, 15:04 UTC")
	}
	if len(data.Logo) > 0 {
		config, err := png.DecodeConfig(bytes.NewReader(data.Logo))
		if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 1024 || config.Height > 1024 {
			return nil, ErrRender
		}
		if _, err = png.Decode(bytes.NewReader(data.Logo)); err != nil {
			return nil, ErrRender
		}
		scale := math.Min(32/float64(config.Width), 14/float64(config.Height))
		view.HasLogo = true
		view.LogoWidth = float64(config.Width) * scale
		view.LogoHeight = float64(config.Height) * scale
	}
	specs := selectFonts(data)
	var css, families strings.Builder
	fontData := make(map[string][]byte, len(specs))
	seen := map[string]bool{}
	for _, font := range specs {
		content, err := assets.ReadFile("fonts/" + font.file)
		if err != nil {
			return nil, err
		}
		fontData[font.file] = content
		fmt.Fprintf(
			&css,
			"@font-face {font-family: %s; font-weight: %d; src: url('https://dad-report.invalid/fonts/%s');}\n",
			font.family,
			font.weight,
			font.file,
		)
		if !seen[font.family] {
			if families.Len() > 0 {
				families.WriteString(", ")
			}
			families.WriteString(font.family)
			seen[font.family] = true
		}
	}
	// These CSS values contain only static embedded-asset metadata, never report text.
	view.FontCSS = template.CSS(css.String())
	view.FontFamilies = template.CSS(families.String())
	parsed, err := reportTemplate()
	if err != nil {
		return nil, err
	}
	var html bytes.Buffer
	if err = parsed.Execute(&html, view); err != nil {
		return nil, err
	}
	fetch := assetFetcher(fontData, data.Logo)
	input, err := tree.NewHTML(utils.InputString(html.String()), "https://dad-report.invalid/", fetch, "print")
	if err != nil {
		return nil, err
	}
	diagnostics := &fontDiagnostics{}
	config := text.NewFontConfigurationGotext(fontscan.NewFontMap(log.New(diagnostics, "", 0)))
	defer func() {
		if count := diagnostics.count.Load(); count > 0 {
			slog.WarnContext(ctx, "PDF used missing-glyph fallback", "diagnostics", count)
		}
	}()
	layout := document.Render(input, nil, false, config)
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	target := &semanticOutput{Output: wpdf.NewOutput()}
	layout.Write(target, 1, nil)
	if target.err != nil {
		return nil, target.err
	}
	result := target.Finalize()
	if err = target.enhance(ctx, &result); err != nil {
		return nil, err
	}
	result.Trailer.Info.CreationDate = data.ExportedAt.UTC()
	result.Trailer.Info.ModDate = data.ExportedAt.UTC()
	result.Trailer.Info.Producer = "DAD"
	result.Trailer.Info.Creator = "DAD"
	output := &boundedPDF{ctx: ctx}
	if err = result.Write(output, nil); err != nil {
		if output.tooLarge {
			return nil, ErrTooLarge
		}
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func validate(data Data) error {
	if len(data.Spend) > MaxSpendRows || len(data.Metrics) > 16 || len(data.Logo) > 360000 ||
		utf8.RuneCountInString(data.Organization) > 4096 {
		return ErrTooLarge
	}
	total := len(data.Organization)
	for _, metric := range data.Metrics {
		total += len(metric.Label) + len(metric.Value)
	}
	for _, row := range data.Spend {
		if utf8.RuneCountInString(row.Category) > 4096 {
			return ErrTooLarge
		}
		total += len(row.Category) + len(row.Amount)
	}
	if total > 128<<10 {
		return ErrTooLarge
	}
	return nil
}

func textDirection(value string) string {
	for _, r := range value {
		properties, _ := bidi.LookupRune(r)
		switch properties.Class() {
		case bidi.R, bidi.AL:
			return "rtl"
		case bidi.L:
			return "ltr"
		}
	}
	return "ltr"
}

func assetFetcher(fonts map[string][]byte, logo []byte) utils.UrlFetcher {
	return func(url string) (utils.RemoteRessource, error) {
		if url == "https://dad-report.invalid/logo.png" && len(logo) > 0 {
			return utils.RemoteRessource{Content: bytes.NewReader(logo), MimeType: "image/png"}, nil
		}
		name, ok := strings.CutPrefix(url, "https://dad-report.invalid/fonts/")
		if content, found := fonts[name]; ok && found {
			return utils.RemoteRessource{Content: bytes.NewReader(content), MimeType: "font/ttf"}, nil
		}
		return utils.RemoteRessource{}, errors.New("report resource is not an embedded asset")
	}
}

type fontDiagnostics struct{ count atomic.Int64 }

func (diagnostics *fontDiagnostics) Write(message []byte) (int, error) {
	diagnostics.count.Add(1)
	return len(message), nil
}

type boundedPDF struct {
	bytes.Buffer
	ctx      context.Context
	tooLarge bool
}

func (output *boundedPDF) Write(data []byte) (int, error) {
	if err := output.ctx.Err(); err != nil {
		return 0, err
	}
	if output.Len()+len(data) > MaxPDFBytes {
		output.tooLarge = true
		return 0, ErrTooLarge
	}
	return output.Buffer.Write(data)
}
