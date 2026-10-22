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
		cfg: Config{ForwardHeader: "X-authentik-username", ForwardGroupsHeader: "X-authentik-groups",
			ForwardSecret: testSecret},
	}
}

const testSecret = "0123456789abcdef0123456789abcdef"

// reqWith builds a request as the proxy would forward it: with the proxy secret whenever it
// carries a forward-auth identity.
func reqWith(headers map[string]string) *http.Request {
	r := rawReq(headers)
	if r.Header.Get("X-authentik-username") != "" {
		r.Header.Set(ForwardSecretHeader, testSecret)
	}
	return r
}

// rawReq builds a request exactly as given — what anyone reaching Belay's port can send.
func rawReq(headers map[string]string) *http.Request {
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

// The vulnerability this guards: Belay listens beyond the proxy's reach (host port, VPN address,
// a Docker network shared with other apps), so a bare identity header must not log anyone in.
func TestForwardedUserNeedsProxySecret(t *testing.T) {
	s := gateServer(t, "")
	for name, tc := range map[string]struct {
		headers map[string]string
		want    string
	}{
		"spoofed header, no secret": {map[string]string{"X-authentik-username": "admin"}, ""},
		"wrong secret": {map[string]string{"X-authentik-username": "admin",
			ForwardSecretHeader: "guess"}, ""},
		"secret prefix only": {map[string]string{"X-authentik-username": "admin",
			ForwardSecretHeader: testSecret[:16]}, ""},
		"through the proxy": {map[string]string{"X-authentik-username": "sergio",
			ForwardSecretHeader: testSecret}, "sergio"},
	} {
		if got := s.user(rawReq(tc.headers)); got != tc.want {
			t.Errorf("%s: user = %q, want %q", name, got, tc.want)
		}
	}
}

func TestForwardHeaderIgnoredWithoutConfiguredSecret(t *testing.T) {
	s := gateServer(t, "")
	s.cfg.ForwardSecret = ""
	r := rawReq(map[string]string{"X-authentik-username": "admin", ForwardSecretHeader: ""})
	if got := s.user(r); got != "" {
		t.Errorf("with no secret configured the header must be ignored, got user %q", got)
	}
}

// A spoofed identity must not count as a forward-auth caller for the group gate either: it falls
// back to needing a password session, which guard checks.
func TestGroupGateIgnoresSpoofedIdentity(t *testing.T) {
	s := gateServer(t, "belay-admins")
	r := rawReq(map[string]string{"X-authentik-username": "admin", "X-authentik-groups": "belay-admins"})
	if s.user(r) != "" {
		t.Fatal("spoofed identity was accepted")
	}
}
