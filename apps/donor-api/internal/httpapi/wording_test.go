package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dad/donor-api/internal/store"
)

type rulesSource struct {
	WordingSource
	saved store.RecognitionRules
}

func (s *rulesSource) SetRules(_ context.Context, _ string, in store.RecognitionRules) error {
	s.saved = in
	return nil
}

func TestRecognitionArtworkBatchBodyLimit(t *testing.T) {
	picture := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	random := rand.New(rand.NewPCG(1, 2))
	for i := range picture.Pix {
		picture.Pix[i] = byte(random.Uint32())
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	artwork := "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
	input := store.RecognitionRules{Milestones: []int64{7, 30}}
	for range 24 {
		input.Badges = append(
			input.Badges,
			store.BadgeRule{Code: "7", Signal: "streak_days", Threshold: 7, MinPopulation: 1, Artwork: artwork},
		)
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= maxJSONBody {
		t.Fatal("fixture does not exceed the ordinary body limit")
	}
	source := &rulesSource{}
	server := NewServer("", Services{Wording: source, Origin: testPublicOrigin, StaffOrigin: testStaffOrigin})
	request := httptest.NewRequest("POST", "/donor/staff/content/rules", bytes.NewReader(body))
	request.Header.Set("Origin", testStaffOrigin)
	request.AddCookie(&http.Cookie{Name: "dad_staff_session", Value: "staff-token"})
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(source.saved.Badges) != len(input.Badges) || source.saved.Badges[0].Artwork != artwork {
		t.Fatalf("artwork batch rejected before validation: %d %s", response.Code, response.Body)
	}
	request = httptest.NewRequest("POST", "/donor/staff/content/rules", strings.NewReader("{}"))
	request.Header.Set("Origin", testStaffOrigin)
	request.AddCookie(&http.Cookie{Name: "dad_staff_session", Value: "staff-token"})
	request.ContentLength = (10 << 20) + 1
	response = httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized batch: %d", response.Code)
	}
}
