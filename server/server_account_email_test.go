package server

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"heckel.io/ntfy/v2/model"
	"heckel.io/ntfy/v2/user"
	"heckel.io/ntfy/v2/util"
)

// captureMailer is a fake mailer that records the magic links it is asked to send, so tests can
// "click" them without a real SMTP server. The notification side is a no-op.
type captureMailer struct {
	verifyLinks map[string]string // email -> verification link
	resetLinks  map[string]string // email -> reset link
}

func newCaptureMailer() *captureMailer {
	return &captureMailer{verifyLinks: map[string]string{}, resetLinks: map[string]string{}}
}

func (c *captureMailer) SendEmailVerification(to, link string) error {
	c.verifyLinks[to] = link
	return nil
}

func (c *captureMailer) SendPasswordReset(to, link string) error {
	c.resetLinks[to] = link
	return nil
}

func (c *captureMailer) SendNotification(to string, m *model.Message, senderIP string) error {
	return nil
}

func (c *captureMailer) NotificationCounts() (total int64, success int64, failure int64) {
	return 0, 0, 0
}

// newEmailTestServer creates a server with email sending "enabled" (SMTP + base-url configured)
// and a capturing mailer injected, plus a tier-less user "ben" logged in via basic auth.
func newEmailTestServer(t *testing.T, databaseURL string) (*Server, *captureMailer, map[string]string) {
	conf := newTestConfigWithAuthFile(t, databaseURL)
	conf.SMTPSenderAddr = "localhost:25"
	conf.SMTPSenderFrom = "noreply@example.com"
	conf.BaseURL = "https://ntfy.example.com"
	s := newTestServer(t, conf)
	mailer := newCaptureMailer()
	s.mailer = mailer
	require.Nil(t, s.userManager.AddUser("ben", "ben", user.RoleUser, false))
	auth := map[string]string{"Authorization": util.BasicAuth("ben", "ben")}
	return s, mailer, auth
}

func getAccount(t *testing.T, s *Server, auth map[string]string) *apiAccountResponse {
	rr := request(t, s, "GET", "/v1/account", "", auth)
	require.Equal(t, 200, rr.Code)
	account, err := util.UnmarshalJSON[apiAccountResponse](io.NopCloser(rr.Body))
	require.Nil(t, err)
	return account
}

// verifiedAddrs / pendingAddrs / primaryAddr extract the addresses from the structured email
// list returned by GET /v1/account, so assertions stay readable.
func verifiedAddrs(account *apiAccountResponse) []string {
	addrs := make([]string, 0)
	for _, e := range account.Emails {
		if !e.Pending {
			addrs = append(addrs, e.Address)
		}
	}
	return addrs
}

func pendingAddrs(account *apiAccountResponse) []string {
	addrs := make([]string, 0)
	for _, e := range account.Emails {
		if e.Pending {
			addrs = append(addrs, e.Address)
		}
	}
	return addrs
}

func primaryAddr(account *apiAccountResponse) string {
	for _, e := range account.Emails {
		if e.Primary {
			return e.Address
		}
	}
	return ""
}

func tokenFromLink(t *testing.T, link, prefix string) string {
	require.True(t, strings.HasPrefix(link, prefix), "link %q missing prefix %q", link, prefix)
	return strings.TrimPrefix(link, prefix)
}

func TestAccount_Email_AddVerifySetsPrimary(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, mailer, auth := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()

		// Start verification
		rr := request(t, s, "PUT", "/v1/account/email", `{"email":"ben@example.com"}`, auth)
		require.Equal(t, 200, rr.Code)

		// Pending, not yet verified, no primary
		account := getAccount(t, s, auth)
		require.Equal(t, []string{"ben@example.com"}, pendingAddrs(account))
		require.Empty(t, verifiedAddrs(account))
		require.Equal(t, "", primaryAddr(account))

		// "Click" the captured link (unauthenticated POST)
		token := tokenFromLink(t, mailer.verifyLinks["ben@example.com"], "https://ntfy.example.com/account/email/verify/")
		rr = request(t, s, "POST", "/v1/account/email/verify", fmt.Sprintf(`{"token":"%s"}`, token), nil)
		require.Equal(t, 200, rr.Code)

		// Now verified + primary, no longer pending
		account = getAccount(t, s, auth)
		require.Equal(t, []string{"ben@example.com"}, verifiedAddrs(account))
		require.Equal(t, "ben@example.com", primaryAddr(account))
		require.Empty(t, pendingAddrs(account))
	})
}

func TestAccount_Email_VerifyInvalidToken(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, _, _ := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()

		rr := request(t, s, "POST", "/v1/account/email/verify", `{"token":"doesnotexist"}`, nil)
		require.Equal(t, 400, rr.Code)
		require.Equal(t, 40051, toHTTPError(t, rr.Body.String()).Code)

		// Empty token also rejected
		rr = request(t, s, "POST", "/v1/account/email/verify", `{"token":""}`, nil)
		require.Equal(t, 400, rr.Code)
	})
}

func TestAccount_Email_DeletePending(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, _, auth := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()

		require.Equal(t, 200, request(t, s, "PUT", "/v1/account/email", `{"email":"ben@example.com"}`, auth).Code)
		require.Equal(t, []string{"ben@example.com"}, pendingAddrs(getAccount(t, s, auth)))

		// Deleting the pending address clears it (no verification ever happened)
		require.Equal(t, 200, request(t, s, "DELETE", "/v1/account/email", `{"email":"ben@example.com"}`, auth).Code)
		account := getAccount(t, s, auth)
		require.Empty(t, pendingAddrs(account))
		require.Empty(t, verifiedAddrs(account))
	})
}

func TestAccount_Email_Resend(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, mailer, auth := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()

		require.Equal(t, 200, request(t, s, "PUT", "/v1/account/email", `{"email":"ben@example.com"}`, auth).Code)
		firstLink := mailer.verifyLinks["ben@example.com"]
		require.NotEmpty(t, firstLink)

		// Resend issues a fresh link (the old one is replaced)
		require.Equal(t, 200, request(t, s, "POST", "/v1/account/email/resend", `{"email":"ben@example.com"}`, auth).Code)
		require.NotEqual(t, firstLink, mailer.verifyLinks["ben@example.com"])

		// The old token no longer verifies; the new one does
		oldToken := tokenFromLink(t, firstLink, "https://ntfy.example.com/account/email/verify/")
		require.Equal(t, 400, request(t, s, "POST", "/v1/account/email/verify", fmt.Sprintf(`{"token":"%s"}`, oldToken), nil).Code)
		newToken := tokenFromLink(t, mailer.verifyLinks["ben@example.com"], "https://ntfy.example.com/account/email/verify/")
		require.Equal(t, 200, request(t, s, "POST", "/v1/account/email/verify", fmt.Sprintf(`{"token":"%s"}`, newToken), nil).Code)

		// Resending for a non-pending address is rejected
		require.Equal(t, 400, request(t, s, "POST", "/v1/account/email/resend", `{"email":"never@example.com"}`, auth).Code)
	})
}

func TestAccount_Email_SetPrimaryCollision(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, mailer, auth := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()

		// ben verifies shared@ -> becomes his primary
		require.Equal(t, 200, request(t, s, "PUT", "/v1/account/email", `{"email":"shared@example.com"}`, auth).Code)
		benToken := tokenFromLink(t, mailer.verifyLinks["shared@example.com"], "https://ntfy.example.com/account/email/verify/")
		require.Equal(t, 200, request(t, s, "POST", "/v1/account/email/verify", fmt.Sprintf(`{"token":"%s"}`, benToken), nil).Code)
		require.Equal(t, "shared@example.com", primaryAddr(getAccount(t, s, auth)))

		// alice verifies the same address -> allowed as secondary, but it is not her primary
		require.Nil(t, s.userManager.AddUser("alice", "alice", user.RoleUser, false))
		aliceAuth := map[string]string{"Authorization": util.BasicAuth("alice", "alice")}
		require.Equal(t, 200, request(t, s, "PUT", "/v1/account/email", `{"email":"shared@example.com"}`, aliceAuth).Code)
		aliceToken := tokenFromLink(t, mailer.verifyLinks["shared@example.com"], "https://ntfy.example.com/account/email/verify/")
		require.Equal(t, 200, request(t, s, "POST", "/v1/account/email/verify", fmt.Sprintf(`{"token":"%s"}`, aliceToken), nil).Code)
		aliceAccount := getAccount(t, s, aliceAuth)
		require.Equal(t, []string{"shared@example.com"}, verifiedAddrs(aliceAccount))
		require.Equal(t, "", primaryAddr(aliceAccount))

		// alice trying to promote it to primary collides with ben's
		rr := request(t, s, "POST", "/v1/account/email/primary", `{"email":"shared@example.com"}`, aliceAuth)
		require.Equal(t, 409, rr.Code)
		require.Equal(t, 40908, toHTTPError(t, rr.Body.String()).Code)
	})
}

// verifyEmailFor runs the full add->click flow so the user ends up with a verified primary email.
func verifyEmailFor(t *testing.T, s *Server, mailer *captureMailer, auth map[string]string, email string) {
	require.Equal(t, 200, request(t, s, "PUT", "/v1/account/email", fmt.Sprintf(`{"email":"%s"}`, email), auth).Code)
	token := tokenFromLink(t, mailer.verifyLinks[email], "https://ntfy.example.com/account/email/verify/")
	require.Equal(t, 200, request(t, s, "POST", "/v1/account/email/verify", fmt.Sprintf(`{"token":"%s"}`, token), nil).Code)
}

// canLogin returns true if username/password authenticates (via the token-create endpoint).
func canLogin(t *testing.T, s *Server, username, password string) bool {
	rr := request(t, s, "POST", "/v1/account/token", "", map[string]string{"Authorization": util.BasicAuth(username, password)})
	return rr.Code == 200
}

func TestAccount_LoginByPrimaryEmail(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, mailer, auth := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()
		verifyEmailFor(t, s, mailer, auth, "ben@example.com")

		// Basic Auth works with either the username or the verified primary email
		require.True(t, canLogin(t, s, "ben", "ben"))
		require.True(t, canLogin(t, s, "ben@example.com", "ben"))

		// ...but not with the wrong password or an unknown email
		require.False(t, canLogin(t, s, "ben@example.com", "wrong"))
		require.False(t, canLogin(t, s, "nobody@example.com", "ben"))
	})
}

func TestAccount_PasswordReset_ByUsername(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, mailer, auth := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()
		verifyEmailFor(t, s, mailer, auth, "ben@example.com")

		// Request reset by username
		rr := request(t, s, "POST", "/v1/account/password/reset/request", `{"identifier":"ben"}`, nil)
		require.Equal(t, 200, rr.Code)
		token := tokenFromLink(t, mailer.resetLinks["ben@example.com"], "https://ntfy.example.com/account/password/reset/")

		// Confirm with a new password
		rr = request(t, s, "POST", "/v1/account/password/reset", fmt.Sprintf(`{"token":"%s","password":"brandnew"}`, token), nil)
		require.Equal(t, 200, rr.Code)

		require.True(t, canLogin(t, s, "ben", "brandnew"))
		require.False(t, canLogin(t, s, "ben", "ben"))
	})
}

func TestAccount_PasswordReset_ByEmail(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, mailer, auth := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()
		verifyEmailFor(t, s, mailer, auth, "ben@example.com")

		rr := request(t, s, "POST", "/v1/account/password/reset/request", `{"identifier":"ben@example.com"}`, nil)
		require.Equal(t, 200, rr.Code)
		token := tokenFromLink(t, mailer.resetLinks["ben@example.com"], "https://ntfy.example.com/account/password/reset/")
		rr = request(t, s, "POST", "/v1/account/password/reset", fmt.Sprintf(`{"token":"%s","password":"brandnew"}`, token), nil)
		require.Equal(t, 200, rr.Code)
		require.True(t, canLogin(t, s, "ben", "brandnew"))
	})
}

func TestAccount_PasswordReset_EmailLookalikeUsernameDoesNotShadow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, mailer, auth := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()

		// Account A (the email owner): user "ben" with verified primary email "phil@example.com"
		verifyEmailFor(t, s, mailer, auth, "phil@example.com")

		// Account B (the squatter): a different account whose USERNAME looks like A's email, with
		// its own, different verified primary email
		require.Nil(t, s.userManager.AddUser("phil@example.com", "squatterpass", user.RoleUser, false))
		squatter, err := s.userManager.User("phil@example.com")
		require.Nil(t, err)
		require.Nil(t, s.userManager.AddEmail(squatter.ID, "squatter@example.com"))
		require.Nil(t, s.userManager.SetPrimaryEmail(squatter.ID, "squatter@example.com"))

		// Reset by the ambiguous identifier: the verified email must win over the look-alike username
		rr := request(t, s, "POST", "/v1/account/password/reset/request", `{"identifier":"phil@example.com"}`, nil)
		require.Equal(t, 200, rr.Code)
		require.NotEmpty(t, mailer.resetLinks["phil@example.com"])  // sent to the email owner (account A)
		require.Empty(t, mailer.resetLinks["squatter@example.com"]) // NOT the username squatter (account B)

		// The token resets account A (ben); the squatter's password is untouched
		token := tokenFromLink(t, mailer.resetLinks["phil@example.com"], "https://ntfy.example.com/account/password/reset/")
		rr = request(t, s, "POST", "/v1/account/password/reset", fmt.Sprintf(`{"token":"%s","password":"brandnew"}`, token), nil)
		require.Equal(t, 200, rr.Code)
		require.True(t, canLogin(t, s, "ben", "brandnew"))                  // account A was reset
		require.True(t, canLogin(t, s, "phil@example.com", "squatterpass")) // account B unaffected
	})
}

func TestAccount_PasswordReset_UnknownIdentifierUniform(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, mailer, _ := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()

		// Unknown identifier still returns a uniform 200, and no email is sent
		rr := request(t, s, "POST", "/v1/account/password/reset/request", `{"identifier":"ghost"}`, nil)
		require.Equal(t, 200, rr.Code)
		require.Empty(t, mailer.resetLinks)
	})
}

func TestAccount_PasswordReset_NoPrimaryEmailNoSend(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, mailer, _ := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()

		// ben exists but has no verified primary email -> uniform 200, nothing sent
		rr := request(t, s, "POST", "/v1/account/password/reset/request", `{"identifier":"ben"}`, nil)
		require.Equal(t, 200, rr.Code)
		require.Empty(t, mailer.resetLinks)
	})
}

func TestAccount_Signup_WithEmail_SendsVerification(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		conf := newTestConfigWithAuthFile(t, databaseURL)
		conf.EnableSignup = true
		conf.SMTPSenderAddr = "localhost:25"
		conf.SMTPSenderFrom = "noreply@example.com"
		conf.BaseURL = "https://ntfy.example.com"
		s := newTestServer(t, conf)
		mailer := newCaptureMailer()
		s.mailer = mailer
		defer s.closeDatabases()

		// Sign up with an optional email -> account created and a verification link sent
		rr := request(t, s, "POST", "/v1/account", `{"username":"emma","password":"emmapass","email":"emma@example.com"}`, nil)
		require.Equal(t, 200, rr.Code)
		link := mailer.verifyLinks["emma@example.com"]
		require.NotEmpty(t, link)

		// Verifying the link makes it the (first) primary email
		token := tokenFromLink(t, link, "https://ntfy.example.com/account/email/verify/")
		require.Equal(t, 200, request(t, s, "POST", "/v1/account/email/verify", fmt.Sprintf(`{"token":"%s"}`, token), nil).Code)
		account := getAccount(t, s, map[string]string{"Authorization": util.BasicAuth("emma", "emmapass")})
		require.Equal(t, []string{"emma@example.com"}, verifiedAddrs(account))
		require.Equal(t, "emma@example.com", primaryAddr(account))
	})
}

func TestAccount_Signup_WithoutEmail_NoSend(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		conf := newTestConfigWithAuthFile(t, databaseURL)
		conf.EnableSignup = true
		conf.SMTPSenderAddr = "localhost:25"
		conf.SMTPSenderFrom = "noreply@example.com"
		conf.BaseURL = "https://ntfy.example.com"
		s := newTestServer(t, conf)
		mailer := newCaptureMailer()
		s.mailer = mailer
		defer s.closeDatabases()

		// No email -> account created, nothing sent
		require.Equal(t, 200, request(t, s, "POST", "/v1/account", `{"username":"emma","password":"emmapass"}`, nil).Code)
		require.Empty(t, mailer.verifyLinks)

		// Invalid email -> rejected
		rr := request(t, s, "POST", "/v1/account", `{"username":"otto","password":"ottopass","email":"not-an-email"}`, nil)
		require.Equal(t, 400, rr.Code)
		require.Equal(t, 40050, toHTTPError(t, rr.Body.String()).Code)
	})
}

func TestAccount_Email_ProvisionedPrimary(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		hash, err := user.HashPassword("provpass", user.DefaultUserPasswordBcryptCost)
		require.Nil(t, err)
		conf := newTestConfigWithAuthFile(t, databaseURL)
		conf.SMTPSenderAddr = "localhost:25"
		conf.SMTPSenderFrom = "noreply@example.com"
		conf.BaseURL = "https://ntfy.example.com"
		conf.AuthUsers = []*user.User{{Name: "prov", Hash: hash, Role: user.RoleUser}}
		s := newTestServer(t, conf)
		mailer := newCaptureMailer()
		s.mailer = mailer
		defer s.closeDatabases()
		auth := map[string]string{"Authorization": util.BasicAuth("prov", "provpass")}

		// A provisioned user's first verified email becomes their primary (used by X-Email: yes;
		// password reset stays blocked separately for provisioned users)
		verifyEmailFor(t, s, mailer, auth, "prov@example.com")
		account := getAccount(t, s, auth)
		require.Equal(t, []string{"prov@example.com"}, verifiedAddrs(account))
		require.Equal(t, "prov@example.com", primaryAddr(account))

		// Verify a second address and explicitly set it primary -> allowed, star moves
		verifyEmailFor(t, s, mailer, auth, "prov2@example.com")
		rr := request(t, s, "POST", "/v1/account/email/primary", `{"email":"prov2@example.com"}`, auth)
		require.Equal(t, 200, rr.Code)
		account = getAccount(t, s, auth)
		require.Equal(t, "prov2@example.com", primaryAddr(account))
	})
}

func TestAccount_PasswordReset_ProvisionedUserNoSend(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		// Provision a user via config (AuthUsers), with email sending enabled
		conf := newTestConfigWithAuthFile(t, databaseURL)
		conf.SMTPSenderAddr = "localhost:25"
		conf.SMTPSenderFrom = "noreply@example.com"
		conf.BaseURL = "https://ntfy.example.com"
		conf.AuthUsers = []*user.User{
			{Name: "prov", Hash: "$2a$10$YLiO8U21sX1uhZamTLJXHuxgVC0Z/GKISibrKCLohPgtG7yIxSk4C", Role: user.RoleUser},
		}
		s := newTestServer(t, conf)
		mailer := newCaptureMailer()
		s.mailer = mailer
		defer s.closeDatabases()

		// Give the provisioned user a verified primary email anyway
		prov, err := s.userManager.User("prov")
		require.Nil(t, err)
		require.True(t, prov.Provisioned)
		require.Nil(t, s.userManager.AddEmail(prov.ID, "prov@example.com"))
		require.Nil(t, s.userManager.SetPrimaryEmail(prov.ID, "prov@example.com"))

		// Reset request by username and by email -> uniform 200, but no email sent (can't reset)
		require.Equal(t, 200, request(t, s, "POST", "/v1/account/password/reset/request", `{"identifier":"prov"}`, nil).Code)
		require.Equal(t, 200, request(t, s, "POST", "/v1/account/password/reset/request", `{"identifier":"prov@example.com"}`, nil).Code)
		require.Empty(t, mailer.resetLinks)
	})
}

func TestAccount_PasswordReset_InvalidToken(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, _, _ := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()

		rr := request(t, s, "POST", "/v1/account/password/reset", `{"token":"nope","password":"brandnew"}`, nil)
		require.Equal(t, 400, rr.Code)
		require.Equal(t, 40054, toHTTPError(t, rr.Body.String()).Code)
	})
}

// emailByAddr finds the structured entry for an address in the GET /v1/account response.
func emailByAddr(account *apiAccountResponse, addr string) *apiAccountEmailInfo {
	for _, e := range account.Emails {
		if e.Address == addr {
			return e
		}
	}
	return nil
}

func TestAccount_Email_ProvisionedViaConfig(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		conf := newTestConfigWithAuthFile(t, databaseURL)
		conf.SMTPSenderAddr = "localhost:25"
		conf.SMTPSenderFrom = "noreply@example.com"
		conf.BaseURL = "https://ntfy.example.com"

		// Start once to create a regular, non-provisioned account ("self-signed-up")
		s := newTestServer(t, conf)
		require.Nil(t, s.userManager.AddUser("alice", "alice", user.RoleUser, false))
		s.closeDatabases()

		// Second start: the admin list assigns a verified + recovery email to the account
		conf.AuthEmails = []*user.ProvisionedEmail{
			{Username: "alice", Address: "alice@example.com", Recovery: true},
			{Username: "alice", Address: "alice-work@example.com"},
		}
		s = newTestServer(t, conf)
		mailer := newCaptureMailer()
		s.mailer = mailer
		auth := map[string]string{"Authorization": util.BasicAuth("alice", "alice")}

		account := getAccount(t, s, auth)
		recovery := emailByAddr(account, "alice@example.com")
		require.NotNil(t, recovery)
		require.True(t, recovery.Primary)
		require.True(t, recovery.Provisioned)
		work := emailByAddr(account, "alice-work@example.com")
		require.NotNil(t, work)
		require.False(t, work.Primary)
		require.True(t, work.Provisioned)

		// The provisioned recovery email logs in, and password reset goes to it
		require.True(t, canLogin(t, s, "alice@example.com", "alice"))
		require.Equal(t, 200, request(t, s, "POST", "/v1/account/password/reset/request", `{"identifier":"alice"}`, nil).Code)
		require.NotEmpty(t, mailer.resetLinks["alice@example.com"])
		token := tokenFromLink(t, mailer.resetLinks["alice@example.com"], "https://ntfy.example.com/account/password/reset/")
		require.Equal(t, 200, request(t, s, "POST", "/v1/account/password/reset", fmt.Sprintf(`{"token":"%s","password":"brandnew"}`, token), nil).Code)
		require.True(t, canLogin(t, s, "alice@example.com", "brandnew"))
		auth = map[string]string{"Authorization": util.BasicAuth("alice", "brandnew")}

		// Alice also verifies a private address herself
		verifyEmailFor(t, s, mailer, auth, "alice-personal@example.com")

		// Third start: the work address is delisted; the self-verified one must survive
		s.closeDatabases()
		conf.AuthEmails = []*user.ProvisionedEmail{
			{Username: "alice", Address: "alice@example.com", Recovery: true},
		}
		s = newTestServer(t, conf)
		s.mailer = mailer

		account = getAccount(t, s, auth)
		require.NotNil(t, emailByAddr(account, "alice@example.com"))
		require.Nil(t, emailByAddr(account, "alice-work@example.com")) // managed address revoked
		personal := emailByAddr(account, "alice-personal@example.com")
		require.NotNil(t, personal) // self-verified address kept
		require.False(t, personal.Provisioned)
	})
}

func TestAccount_Email_ProvisionedConflictFailsStartup(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		hash, err := user.HashPassword("pass", user.DefaultUserPasswordBcryptCost)
		require.Nil(t, err)
		conf := newTestConfigWithAuthFile(t, databaseURL)
		conf.AuthUsers = []*user.User{
			{Name: "ben", Hash: hash, Role: user.RoleUser},
			{Name: "alice", Hash: hash, Role: user.RoleUser},
		}
		// Same address assigned to two accounts -> the whole list must be rejected
		conf.AuthEmails = []*user.ProvisionedEmail{
			{Username: "ben", Address: "shared@example.com", Recovery: true},
			{Username: "alice", Address: "shared@example.com"},
		}
		s, err := New(conf)
		require.Error(t, err)
		require.Nil(t, s)
		require.True(t, errors.Is(err, user.ErrEmailProvisioningConflict), "expected provisioning conflict, got %v", err)
		require.Contains(t, err.Error(), "shared@example.com")
	})
}

func TestAccount_Email_AddDuplicateVerified(t *testing.T) {
	forEachBackend(t, func(t *testing.T, databaseURL string) {
		s, mailer, auth := newEmailTestServer(t, databaseURL)
		defer s.closeDatabases()

		require.Equal(t, 200, request(t, s, "PUT", "/v1/account/email", `{"email":"ben@example.com"}`, auth).Code)
		token := tokenFromLink(t, mailer.verifyLinks["ben@example.com"], "https://ntfy.example.com/account/email/verify/")
		require.Equal(t, 200, request(t, s, "POST", "/v1/account/email/verify", fmt.Sprintf(`{"token":"%s"}`, token), nil).Code)

		// Adding the same already-verified address is a conflict
		rr := request(t, s, "PUT", "/v1/account/email", `{"email":"ben@example.com"}`, auth)
		require.Equal(t, 409, rr.Code)
		require.Equal(t, 40907, toHTTPError(t, rr.Body.String()).Code)
	})
}
