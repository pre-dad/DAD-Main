package partnerreport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestUnicodeReportPreservesSourceText(t *testing.T) {
	name := "Café — 東京の支援 · 𠮷 · العربية · עברית · नमस्ते · 한국어 · 🌟 · fi ﬁ Café · 👩‍🚀 · \ue000\ue001"
	data := Data{
		Organization: name,
		ExportedAt:   time.Date(2027, 3, 4, 5, 6, 0, 0, time.UTC),
		Metrics:      []Metric{{Label: "Committed", Value: "$1,500.00"}},
		Spend:        []Spend{{Category: "<img src=\"https://example.invalid/secret\"> 日本語", Amount: "$500.00"}},
	}
	pdf, err := Render(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report.pdf")
	if err = os.WriteFile(path, pdf, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = exec.LookPath("pdftotext"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("pdftotext (Poppler) is required in CI")
		}
		t.Skip("pdftotext (Poppler) not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "pdftotext", "-layout", path, "-").CombinedOutput()
	if err != nil {
		t.Fatalf("extract PDF (install Poppler): %v %s", err, output)
	}
	extracted := strings.Map(func(r rune) rune {
		if r >= 0x202a && r <= 0x202e {
			return -1
		}
		return r
	}, string(output))
	for _, expected := range []string{"Café", "東京の支援", "𠮷", "العربية", "עברית", "नमस्ते", "한국어", "🌟", "fi ﬁ Café", "👩‍🚀", "\ue000\ue001", "<img src=\"https://example.invalid/secret\">", "$1,500.00", "Figures verified by DAD"} {
		if !strings.Contains(extracted, expected) {
			t.Errorf("missing %q in extracted PDF:\n%s", expected, extracted)
		}
	}
	if len(pdf) > 200000 {
		t.Errorf("font subsets unexpectedly large: %d bytes", len(pdf))
	}
}

func TestEmbeddedFontProvenance(t *testing.T) {
	content, err := assets.ReadFile("fonts/provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Assets []struct {
			File, SHA256 string
			LicenseFile  string `json:"license_file"`
		}
	}
	if err = json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{}
	for _, entry := range manifest.Assets {
		expected[entry.File] = entry.SHA256
		if _, err := assets.ReadFile("fonts/" + entry.LicenseFile); err != nil {
			t.Errorf("%s names no embedded license file: %q", entry.File, entry.LicenseFile)
		}
	}
	for _, font := range fontCatalog {
		data, err := assets.ReadFile("fonts/" + font.file)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if expected[font.file] != hex.EncodeToString(digest[:]) {
			t.Errorf("font provenance mismatch: %s", font.file)
		}
	}
	for _, name := range []string{"OFL-Noto.txt", "OFL-NotoEmoji.txt", "OFL-NotoSansCJK.txt"} {
		license, err := assets.ReadFile("fonts/" + name)
		if err != nil || !bytes.Contains(license, []byte("SIL OPEN FONT LICENSE")) {
			t.Errorf("missing font redistribution license %s: %v", name, err)
		}
		if !bytes.HasPrefix(license, []byte("Copyright")) {
			t.Errorf("%s does not begin with the copyright notice the OFL requires", name)
		}
	}
}

func TestRendererRejectsUnboundedInput(t *testing.T) {
	for _, data := range []Data{{Spend: make([]Spend, MaxSpendRows+1)}, {Organization: strings.Repeat("x", 4097)}, {Metrics: []Metric{{Value: strings.Repeat("x", (128<<10)+1)}}}, {Logo: make([]byte, 360001)}} {
		if pdf, err := Render(t.Context(), data); !errors.Is(err, ErrTooLarge) || len(pdf) != 0 {
			t.Errorf("oversize report: bytes=%d err=%v", len(pdf), err)
		}
	}
	if pdf, err := Render(t.Context(), Data{Logo: []byte("not a PNG")}); !errors.Is(err, ErrRender) || len(pdf) != 0 {
		t.Errorf("invalid logo: bytes=%d err=%v", len(pdf), err)
	}
}

func TestQueuedRendererHonorsCancellation(t *testing.T) {
	renderSlot <- struct{}{}
	defer func() { <-renderSlot }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if pdf, err := Render(ctx, Data{}); !errors.Is(err, context.Canceled) || len(pdf) != 0 {
		t.Fatalf("queued cancellation: bytes=%d err=%v", len(pdf), err)
	}
}

func TestAssetFetcherCannotReadExternalResources(t *testing.T) {
	fetch := assetFetcher(map[string][]byte{"font.ttf": []byte("font")}, []byte("logo"))
	for _, url := range []string{"https://example.com/font.ttf", "file:///etc/passwd", "https://dad-report.invalid/fonts/../font.ttf", "https://dad-report.invalid/fonts/font.ttf?token=x"} {
		if _, err := fetch(url); err == nil {
			t.Errorf("accepted resource %s", url)
		}
	}
	if _, err := fetch("https://dad-report.invalid/fonts/font.ttf"); err != nil {
		t.Fatal(err)
	}
}

func TestPDFOutputLimitIsAtomic(t *testing.T) {
	output := &boundedPDF{ctx: t.Context()}
	if n, err := output.Write(make([]byte, MaxPDFBytes)); err != nil || n != MaxPDFBytes {
		t.Fatalf("at limit: %d %v", n, err)
	}
	if n, err := output.Write([]byte{1}); !errors.Is(err, ErrTooLarge) || n != 0 || output.Len() != MaxPDFBytes {
		t.Fatalf("over limit: %d %v", n, err)
	}
}

func TestSelectFontsLoadsOnlyTheFacesAReportNeeds(t *testing.T) {
	families := func(data Data) []string {
		var names []string
		for _, font := range selectFonts(data) {
			if !slices.Contains(names, font.family) {
				names = append(names, font.family)
			}
		}
		return names
	}
	if got := families(Data{Metrics: []Metric{{Label: "Growth", Value: "+12% < 5 = ok ©"}}}); !slices.Equal(got, []string{"ReportLatin"}) {
		t.Errorf("ASCII and Latin-1 symbols selected %v, want Latin only", got)
	}
	got := families(Data{Organization: "東京。 ∑ 🌟"})
	for _, want := range []string{"ReportLatin", "ReportCJK", "ReportEmoji", "ReportSymbols", "ReportMath"} {
		if !slices.Contains(got, want) {
			t.Errorf("CJK punctuation, math and emoji selected %v, missing %s", got, want)
		}
	}
}
