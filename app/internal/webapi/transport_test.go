package webapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
)

// A conformant client (openapi-fetch included — see web/api/README.md's
// verified finding) percent-encodes "/" as "%2F" when substituting a path
// parameter, regardless of this contract's allowReserved marking, which
// @typespec/openapi3 does not currently emit. This test proves the server
// does not depend on a client doing otherwise: routed through a real
// net/http.Server (not httptest.NewRequest, which never round-trips a raw
// request line through URL parsing), both an unencoded "/" and an encoded
// "%2F" must resolve to the same, correct, full session name.
func TestTransport_SlashInSessionNameSurvivesBothRawAndPercentEncodedForm(t *testing.T) {
	tests := []struct {
		name       string
		requestURI string
	}{
		{"unencoded slash", "/sessions/team/workspace-a"},
		{"percent-encoded slash", "/sessions/team%2Fworkspace-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeReader{status: &service.StatusResult{
				Identity: service.StatusIdentity{SessionName: "team/workspace-a", CreatedAt: time.Now()},
				Runtime:  service.StatusRuntime{Run: domain.RunUp},
			}}
			srv := httptest.NewServer(Routes(svc))
			defer srv.Close()

			resp, err := http.Get(srv.URL + tt.requestURI)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.requestURI, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if svc.gotName != "team/workspace-a" {
				t.Errorf("Status called with %q, want the full slash-containing name", svc.gotName)
			}
		})
	}
}
