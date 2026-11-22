package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dad/donor-api/internal/store"
)

type uploadStub struct {
	ContentAdminSource
	authorizeErr error
	stored       int
}

func (s *uploadStub) AuthorizeSourceUpload(context.Context, string) error { return s.authorizeErr }
func (s *uploadStub) StoreSource(_ context.Context, _, name string, data []byte) (store.StoredSource, error) {
	s.stored++
	return store.StoredSource{Name: name}, nil
}

type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("upload body read before storage was known to be configured")
	return 0, io.EOF
}

func TestSourceUploadWithoutStorageKeyIsRejectedBeforeBody(t *testing.T) {
	uploadNamed := func(stub *uploadStub, name string, body io.Reader) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/donor/staff/content/sources?name="+name, body)
		r.Header.Set("Origin", testStaffOrigin)
		r.AddCookie(&http.Cookie{Name: staffCookie, Value: "staff-session"})
		response := httptest.NewRecorder()
		NewServer("", Services{ContentAdmin: stub}).Handler.ServeHTTP(response, r)
		return response
	}
	upload := func(stub *uploadStub, body io.Reader) *httptest.ResponseRecorder {
		return uploadNamed(stub, "source.pdf", body)
	}
	stub := &uploadStub{authorizeErr: store.ErrSourceStorageUnconfigured}
	response := upload(stub, unreadBody{t})
	if response.Code != 503 || !strings.Contains(response.Body.String(), `"code":"SOURCE_STORAGE_UNCONFIGURED"`) || stub.stored != 0 {
		t.Fatalf("unconfigured storage: %d %s", response.Code, response.Body)
	}
	stub = &uploadStub{authorizeErr: store.ErrStaffForbidden}
	if response = upload(stub, unreadBody{t}); response.Code != 403 {
		t.Fatalf("forbidden upload: %d", response.Code)
	}
	stub = &uploadStub{}
	if response = upload(stub, strings.NewReader("signed record")); response.Code != 200 || stub.stored != 1 {
		t.Fatalf("configured upload: %d %s", response.Code, response.Body)
	}
	stub = &uploadStub{}
	if response = uploadNamed(stub, "a%00b.pdf", unreadBody{t}); response.Code != 422 || stub.stored != 0 {
		t.Fatalf("NUL in the name: %d %s", response.Code, response.Body)
	}
	stub = &uploadStub{}
	if response = upload(stub, strings.NewReader(strings.Repeat("x", maxSourceBytes+1))); response.Code != 413 || stub.stored != 0 {
		t.Fatalf("oversize upload: %d %s", response.Code, response.Body)
	}
}
