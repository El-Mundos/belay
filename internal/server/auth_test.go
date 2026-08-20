package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/El-Mundos/belay/internal/config"
)

func gateServer(t *testing.T, requireGroup string) *Server {
	t.Helper()
	set, _ := config.Open("")
	if err := set.Update(func(s *config.Settings) { s.RequireGroup = requireGroup }); err != nil {
		t.Fatal(err)
	}
	return &Server{
		set: set,
		cfg: Config{ForwardHeader: "X-authentik-username", ForwardGroupsHeader: "X-authentik-groups"},
	}
}

func reqWith(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestGroupAllowed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		require string
		headers map[string]string
		want    bool
	}{
		{"no group configured lets anyone through", "", map[string]string{
			"X-authentik-username": "sergio"}, true},
		{"member of the required group", "belay-admins", map[string]string{
			"X-authentik-username": "sergio", "X-authentik-groups": "authentik Admins|belay-admins"}, true},
		{"authenticated but not a member", "belay-admins", map[string]string{
			"X-authentik-username": "someone", "X-authentik-groups": "users|guests"}, false},
		// The whole point: forward-auth alone must no longer be sufficient.
		{"no groups header at all fails closed", "belay-admins", map[string]string{
			"X-authentik-username": "someone"}, false},
		{"empty groups header fails closed", "belay-admins", map[string]string{
			"X-authentik-username": "someone", "X-authentik-groups": ""}, false},
		// A password session carries no forward-auth header, and must keep working so a
		// misconfigured proxy can't lock the admin out entirely.
		{"password session is exempt", "belay-admins", nil, true},
		{"comma-separated groups also parse", "belay-admins", map[string]string{
			"X-authentik-username": "sergio", "X-authentik-groups": "users, belay-admins"}, true},
		{"match is case-insensitive", "Belay-Admins", map[string]string{
			"X-authentik-username": "sergio", "X-authentik-groups": "belay-admins"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := gateServer(t, tc.require)
			if got := s.groupAllowed(reqWith(tc.headers)); got != tc.want {
				t.Errorf("groupAllowed = %v, want %v", got, tc.want)
			}
		})
	}
}

// Without a forward header configured at all the gate cannot be enforced, so it must not lock out
// an install that only uses the password login.
func TestGroupAllowed_NoForwardAuthConfigured(t *testing.T) {
	s := gateServer(t, "belay-admins")
	s.cfg.ForwardHeader = ""
	if !s.groupAllowed(reqWith(nil)) {
		t.Error("group gate must be inert when forward-auth is not configured")
	}
}
